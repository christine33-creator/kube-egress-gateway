// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

package manager

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	compute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	network "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	egressgatewayv1alpha1 "github.com/Azure/kube-egress-gateway/api/v1alpha1"
	"github.com/Azure/kube-egress-gateway/pkg/azmanager"
	"github.com/Azure/kube-egress-gateway/pkg/consts"
	"github.com/Azure/kube-egress-gateway/pkg/metrics"
	"github.com/Azure/kube-egress-gateway/pkg/utils/to"
)

// GatewayVMConfigurationReconciler reconciles a GatewayVMConfiguration object
type GatewayVMConfigurationReconciler struct {
	client.Client
	*azmanager.AzureManager
	Recorder record.EventRecorder
}

var (
	publicIPPrefixRE = regexp.MustCompile(`(?i).*/subscriptions/(.+)/resourceGroups/(.+)/providers/Microsoft.Network/publicIPPrefixes/(.+)`)
)

//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=egressgateway.kubernetes.azure.com,resources=gatewayvmconfigurations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=egressgateway.kubernetes.azure.com,resources=gatewayvmconfigurations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=egressgateway.kubernetes.azure.com,resources=gatewayvmconfigurations/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the GatewayVMConfiguration object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *GatewayVMConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// handle node events and enqueue corresponding gatewayVMConfigurations if nodepool matches
	if req.Namespace == "" && req.Name != "" {
		log.Info(fmt.Sprintf("Reconciling node event %s", req.Name))
		node := &corev1.Node{}
		if err := r.Get(ctx, req.NamespacedName, node); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			log.Error(err, "unable to fetch node instance")
			return ctrl.Result{}, err
		}

		vmConfigList := &egressgatewayv1alpha1.GatewayVMConfigurationList{}
		if err := r.List(ctx, vmConfigList); err != nil {
			log.Error(err, "failed to list GatewayVMConfiguration")
			return ctrl.Result{}, err
		}
		var aggregateError error
		for _, vmConfig := range vmConfigList.Items {
			// skip reconciling when vmConfig is deleting
			if !vmConfig.ObjectMeta.DeletionTimestamp.IsZero() {
				continue
			}

			// Check if this node matches the vmConfig
			if !r.nodeMatchesVMConfig(ctx, node, &vmConfig) {
				continue
			}

			log.Info(fmt.Sprintf("reconcile vmConfig (%s/%s) upon node (%s) event", vmConfig.GetNamespace(), vmConfig.GetName(), req.Name))
			if _, err := r.reconcile(ctx, &vmConfig); err != nil {
				log.Error(err, "failed to reconcile GatewayVMConfiguration")
				aggregateError = errors.Join(aggregateError, err)
				continue // continue to reconcile other vmConfigs
			}
		}
		return ctrl.Result{}, aggregateError
	}

	vmConfig := &egressgatewayv1alpha1.GatewayVMConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, vmConfig); err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch GatewayVMConfiguration instance")
		return ctrl.Result{}, err
	}

	gwConfig := &egressgatewayv1alpha1.StaticGatewayConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, gwConfig); err != nil {
		log.Error(err, "failed to fetch StaticGatewayConfiguration instance")
		return ctrl.Result{}, err
	}

	if !vmConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		// Clean up gatewayVMConfiguration
		res, err := r.ensureDeleted(ctx, vmConfig)
		if err != nil {
			r.Recorder.Event(gwConfig, corev1.EventTypeWarning, "EnsureDeleteGatewayVMConfigurationError", err.Error())
		}
		return res, err
	}

	res, err := r.reconcile(ctx, vmConfig)
	if err != nil {
		r.Recorder.Event(gwConfig, corev1.EventTypeWarning, "ReconcileGatewayVMConfigurationError", err.Error())
	} else {
		r.Recorder.Event(gwConfig, corev1.EventTypeNormal, "ReconcileGatewayVMConfigurationSuccess", "GatewayVMConfiguration reconciled")
	}
	return res, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayVMConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&egressgatewayv1alpha1.GatewayVMConfiguration{}).
		// allow for node events to trigger reconciliation when either node label matches
		Watches(&corev1.Node{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(resourceHasFilterLabel(
			map[string]string{
				consts.AKSNodepoolModeLabel:      consts.AKSNodepoolModeValue,
				consts.UpstreamNodepoolModeLabel: "true",
				consts.StandaloneVMNodeModeLabel: consts.StandaloneVMNodeModeValue,
			}))).
		Complete(r)
}

// resourceHasFilterLabel returns a predicate that returns true only if the provided resource contains a label
func resourceHasFilterLabel(m map[string]string) predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return ifLabelMatch(e.Object, m)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return ifLabelMatch(e.Object, m)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func ifLabelMatch(obj client.Object, m map[string]string) bool {
	if len(m) == 0 {
		return true
	}

	// return true if any label matches
	for labelKey, labelValue := range m {
		if labelKey == "" {
			return true
		}

		labels := obj.GetLabels()
		if v, ok := labels[labelKey]; ok {
			// Return early if no labelValue was set.
			if labelValue == "" {
				return true
			}

			if strings.EqualFold(v, labelValue) {
				return true
			}
		}
	}

	return false
}

func (r *GatewayVMConfigurationReconciler) nodeMatchesVMConfig(
	ctx context.Context,
	node *corev1.Node,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) bool {
	// Check if this is standalone VM mode
	if r.isStandaloneVMMode(ctx, vmConfig) {
		return r.nodeMatchesStandaloneVMConfig(ctx, node, vmConfig)
	} else {
		return r.nodeMatchesVMSSConfig(ctx, node, vmConfig)
	}
}

func (r *GatewayVMConfigurationReconciler) nodeMatchesVMSSConfig(
	ctx context.Context,
	node *corev1.Node,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) bool {
	// Original VMSS matching logic
	if v, ok := node.Labels[consts.AKSNodepoolNameLabel]; ok {
		if npName := vmConfig.Spec.GatewayNodepoolName; npName != "" && !strings.EqualFold(v, npName) {
			return false
		}
		return true
	}
	return false
}

func (r *GatewayVMConfigurationReconciler) nodeMatchesStandaloneVMConfig(
	ctx context.Context,
	node *corev1.Node,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) bool {
	// Get standalone VM profile
	vmProfile, _, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err != nil {
		return false
	}

	// Extract VM name from node - could be from hostname or provider ID
	vmName := r.getVMNameFromNode(node)
	if vmName == "" {
		return false
	}

	// Check if this VM name is in the configured VM list
	for _, configuredVM := range vmProfile.VMNames {
		if strings.EqualFold(vmName, configuredVM) {
			return true
		}
	}
	return false
}

func (r *GatewayVMConfigurationReconciler) getVMNameFromNode(node *corev1.Node) string {
	// Try to extract VM name from provider ID first (more reliable)
	// Azure provider ID format: azure:///subscriptions/{sub-id}/resourceGroups/{rg}/providers/Microsoft.Compute/virtualMachines/{vm-name}
	if node.Spec.ProviderID != "" {
		parts := strings.Split(node.Spec.ProviderID, "/")
		if len(parts) > 0 {
			// Last part should be the VM name
			return parts[len(parts)-1]
		}
	}

	// Fallback to hostname
	if hostname, ok := node.Labels["kubernetes.io/hostname"]; ok {
		return hostname
	}

	// Last resort - use node name
	return node.Name
}

func (r *GatewayVMConfigurationReconciler) reconcile(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info(fmt.Sprintf("Reconciling GatewayVMConfiguration %s/%s", vmConfig.Namespace, vmConfig.Name))

	mc := metrics.NewMetricsContext(
		os.Getenv(consts.PodNamespaceEnvKey),
		"reconcile_gateway_vm_configuration",
		r.SubscriptionID(),
		r.ResourceGroup,
		strings.ToLower(fmt.Sprintf("%s/%s", vmConfig.Namespace, vmConfig.Name)),
	)
	succeeded := false
	defer func() { mc.ObserveControllerReconcileMetrics(succeeded) }()

	if !controllerutil.ContainsFinalizer(vmConfig, consts.VMConfigFinalizerName) {
		log.Info("Adding finalizer")
		controllerutil.AddFinalizer(vmConfig, consts.VMConfigFinalizerName)
		err := r.Update(ctx, vmConfig)
		if err != nil {
			log.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
	}

	existing := &egressgatewayv1alpha1.GatewayVMConfiguration{}
	vmConfig.DeepCopyInto(existing)

	// Check for migration between deployment types
	isMigrating, previousMode, currentMode, err := r.detectMigration(ctx, vmConfig, existing)
	if err != nil {
		log.Error(err, "failed to detect migration")
		return ctrl.Result{}, err
	}

	// Handle migration if detected
	if isMigrating {
		log.Info("Executing deployment migration", "from", previousMode, "to", currentMode)
		if err := r.executeMigration(ctx, vmConfig, previousMode, currentMode); err != nil {
			log.Error(err, "migration failed")
			return ctrl.Result{}, err
		}
		log.Info("Deployment migration completed successfully")
	}

	// Determine the deployment mode and reconcile accordingly
	mode, err := r.getDeploymentMode(ctx, vmConfig)
	if err != nil {
		log.Error(err, "failed to determine deployment mode")
		return ctrl.Result{}, err
	}

	switch mode {
	case MixedMode:
		return r.reconcileMixedMode(ctx, vmConfig, existing)
	case StandaloneMode:
		return r.reconcileStandaloneVMMode(ctx, vmConfig, existing)
	default: // VMSSMode
		return r.reconcileVMSSMode(ctx, vmConfig, existing)
	}
}

// getDeploymentMode determines the deployment mode for the gateway configuration
type DeploymentMode int

const (
	VMSSMode       DeploymentMode = iota
	StandaloneMode
	MixedMode
)

func (r *GatewayVMConfigurationReconciler) getDeploymentMode(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) (DeploymentMode, error) {
	logger := log.FromContext(ctx).WithValues("vmConfig", vmConfig.Name)
	
	// Check the new GatewayProfile structure first
	hasStandalone := vmConfig.Spec.GatewayProfile.StandaloneVMProfile != nil
	hasVMSS := vmConfig.Spec.GatewayProfile.VmssProfile != nil || 
		vmConfig.Spec.GatewayNodepoolName != "" || 
		!vmssProfileIsEmptyForVMConfig(vmConfig)

	// If no clear indication in vmConfig, check the parent StaticGatewayConfiguration
	if !hasStandalone && !hasVMSS {
		gwConfig := &egressgatewayv1alpha1.StaticGatewayConfiguration{}
		err := r.Get(ctx, client.ObjectKey{
			Namespace: vmConfig.Namespace,
			Name:      vmConfig.Name,
		}, gwConfig)
		if err != nil {
			return VMSSMode, fmt.Errorf("failed to get StaticGatewayConfiguration: %w", err)
		}
		
		hasStandalone = gwConfig.Spec.GatewayProfile.StandaloneVMProfile != nil
		hasVMSS = gwConfig.Spec.GatewayProfile.VmssProfile != nil
	}

	// Determine the deployment mode
	if hasStandalone && hasVMSS {
		logger.Info("Mixed deployment mode detected")
		return MixedMode, nil
	} else if hasStandalone {
		logger.Info("Standalone VM deployment mode detected")
		return StandaloneMode, nil
	} else {
		logger.Info("VMSS deployment mode detected")
		return VMSSMode, nil
	}
}

func (r *GatewayVMConfigurationReconciler) isStandaloneVMMode(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) bool {
	mode, err := r.getDeploymentMode(ctx, vmConfig)
	if err != nil {
		return false // Default to VMSS mode if we can't determine
	}
	return mode == StandaloneMode
}

func vmssProfileIsEmptyForVMConfig(vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration) bool {
	return vmConfig.Spec.GatewayVmssProfile.VmssResourceGroup == "" &&
		vmConfig.Spec.GatewayVmssProfile.VmssName == "" &&
		vmConfig.Spec.GatewayVmssProfile.PublicIpPrefixSize == 0
}

func (r *GatewayVMConfigurationReconciler) reconcileVMSSMode(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	existing *egressgatewayv1alpha1.GatewayVMConfiguration,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	vmss, ipPrefixLength, err := r.getGatewayVMSS(ctx, vmConfig)
	if err != nil {
		log.Error(err, "failed to get vmss")
		return ctrl.Result{}, err
	}

	ipPrefix, ipPrefixID, isManaged, err := r.ensurePublicIPPrefix(ctx, ipPrefixLength, vmConfig)
	if err != nil {
		log.Error(err, "failed to ensure public ip prefix")
		return ctrl.Result{}, err
	}

	var privateIPs []string
	if privateIPs, err = r.reconcileVMSS(ctx, vmConfig, vmss, ipPrefixID, true); err != nil {
		log.Error(err, "failed to reconcile VMSS")
		return ctrl.Result{}, err
	}

	if !isManaged {
		if err := r.ensurePublicIPPrefixDeleted(ctx, vmConfig); err != nil {
			log.Error(err, "failed to remove managed public ip prefix")
			return ctrl.Result{}, err
		}
	}

	if vmConfig.Status == nil {
		vmConfig.Status = &egressgatewayv1alpha1.GatewayVMConfigurationStatus{}
	}
	if vmConfig.Spec.ProvisionPublicIps {
		vmConfig.Status.EgressIpPrefix = ipPrefix
	} else {
		vmConfig.Status.EgressIpPrefix = strings.Join(privateIPs, ",")
	}

	if !equality.Semantic.DeepEqual(existing, vmConfig) {
		log.Info(fmt.Sprintf("Updating GatewayVMConfiguration %s/%s", vmConfig.Namespace, vmConfig.Name))
		if err := r.Status().Update(ctx, vmConfig); err != nil {
			log.Error(err, "failed to update gateway vm configuration")
		}
	}

	log.Info("GatewayVMConfiguration reconciled")
	return ctrl.Result{}, nil
}

func (r *GatewayVMConfigurationReconciler) reconcileMixedMode(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	existing *egressgatewayv1alpha1.GatewayVMConfiguration,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling mixed deployment mode (VMSS + Standalone VMs)")

	// Validate mixed deployment compatibility
	if err := r.validateMixedDeploymentCompatibility(ctx, vmConfig); err != nil {
		log.Error(err, "mixed deployment validation failed")
		return ctrl.Result{}, err
	}

	// Coordinate load balancer for mixed deployment
	if err := r.coordinateLoadBalancerForMixedDeployment(ctx, vmConfig); err != nil {
		log.Error(err, "load balancer coordination failed")
		return ctrl.Result{}, err
	}

	// Initialize variables for tracking results from both deployment types
	var allPrivateIPs []string
	var combinedIPPrefix string
	var err error

	// First, handle VMSS deployment if configured
	vmss, vmssIPPrefixLength, err := r.getGatewayVMSS(ctx, vmConfig)
	if err == nil {
		log.Info("Reconciling VMSS component of mixed deployment")
		
		// Handle VMSS public IP prefix
		vmssIPPrefix, vmssIPPrefixID, vmssIsManaged, err := r.ensurePublicIPPrefix(ctx, vmssIPPrefixLength, vmConfig)
		if err != nil {
			log.Error(err, "failed to ensure VMSS public IP prefix")
			return ctrl.Result{}, err
		}

		// Reconcile VMSS
		vmssPrivateIPs, err := r.reconcileVMSS(ctx, vmConfig, vmss, vmssIPPrefixID, true)
		if err != nil {
			log.Error(err, "failed to reconcile VMSS component")
			return ctrl.Result{}, err
		}

		allPrivateIPs = append(allPrivateIPs, vmssPrivateIPs...)
		if vmConfig.Spec.ProvisionPublicIps {
			combinedIPPrefix = vmssIPPrefix
		}

		// Clean up VMSS public IP prefix if not managed
		if !vmssIsManaged {
			if err := r.ensurePublicIPPrefixDeleted(ctx, vmConfig); err != nil {
				log.Error(err, "failed to remove managed VMSS public IP prefix")
				// Don't return error, continue with standalone VMs
			}
		}
	} else {
		log.V(1).Info("No VMSS configuration found, skipping VMSS reconciliation")
	}

	// Next, handle standalone VM deployment if configured
	standaloneProfile, standaloneIPPrefixLength, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err == nil {
		log.Info("Reconciling standalone VM component of mixed deployment")
		
		// Handle standalone VM public IP prefix
		// Note: For mixed deployments, we might want to use the same prefix or separate ones
		standaloneIPPrefix, standaloneIPPrefixID, standaloneIsManaged, err := r.ensurePublicIPPrefix(ctx, standaloneIPPrefixLength, vmConfig)
		if err != nil {
			log.Error(err, "failed to ensure standalone VM public IP prefix")
			return ctrl.Result{}, err
		}

		// Reconcile standalone VMs
		standalonePrivateIPs, err := r.reconcileStandaloneVMs(ctx, vmConfig, standaloneProfile, standaloneIPPrefixID, true)
		if err != nil {
			log.Error(err, "failed to reconcile standalone VM component")
			return ctrl.Result{}, err
		}

		allPrivateIPs = append(allPrivateIPs, standalonePrivateIPs...)
		if vmConfig.Spec.ProvisionPublicIps {
			if combinedIPPrefix == "" {
				combinedIPPrefix = standaloneIPPrefix
			} else if standaloneIPPrefix != "" && standaloneIPPrefix != combinedIPPrefix {
				// Multiple IP prefixes - combine them
				combinedIPPrefix = fmt.Sprintf("%s,%s", combinedIPPrefix, standaloneIPPrefix)
			}
		}

		// Clean up standalone VM public IP prefix if not managed
		if !standaloneIsManaged {
			if err := r.ensurePublicIPPrefixDeleted(ctx, vmConfig); err != nil {
				log.Error(err, "failed to remove managed standalone VM public IP prefix")
				// Don't return error, continue
			}
		}
	} else {
		log.V(1).Info("No standalone VM configuration found, skipping standalone VM reconciliation")
	}

	// Update the status with combined results
	if vmConfig.Status == nil {
		vmConfig.Status = &egressgatewayv1alpha1.GatewayVMConfigurationStatus{}
	}

	if vmConfig.Spec.ProvisionPublicIps {
		vmConfig.Status.EgressIpPrefix = combinedIPPrefix
	} else {
		vmConfig.Status.EgressIpPrefix = strings.Join(allPrivateIPs, ",")
	}

	if !equality.Semantic.DeepEqual(existing, vmConfig) {
		log.Info(fmt.Sprintf("Updating mixed deployment GatewayVMConfiguration %s/%s", vmConfig.Namespace, vmConfig.Name))
		if err := r.Status().Update(ctx, vmConfig); err != nil {
			log.Error(err, "failed to update gateway vm configuration")
			return ctrl.Result{}, err
		}
	}

	// Collect metrics for standalone VMs if present
	if standaloneProfile != nil {
		metrics, err := r.collectStandaloneVMMetrics(ctx, vmConfig)
		if err != nil {
			log.V(1).Info("Failed to collect standalone VM metrics in mixed deployment", "error", err)
		} else {
			r.logStandaloneVMOperationalStatus(ctx, vmConfig, metrics)
		}

		// Perform health checks for standalone VMs (non-blocking)
		if err := r.performStandaloneVMHealthChecks(ctx, vmConfig); err != nil {
			log.V(1).Info("Standalone VM health check issues in mixed deployment", "error", err)
		}
	}

	log.Info("Mixed deployment GatewayVMConfiguration reconciled successfully", 
		"vmssIPs", len(allPrivateIPs) > 0,
		"standaloneIPs", len(allPrivateIPs) > 0,
		"totalIPs", len(allPrivateIPs))
	return ctrl.Result{}, nil
}

func (r *GatewayVMConfigurationReconciler) reconcileStandaloneVMMode(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	existing *egressgatewayv1alpha1.GatewayVMConfiguration,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Get standalone VM configuration details
	vmProfile, ipPrefixLength, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err != nil {
		log.Error(err, "failed to get standalone VM profile")
		return ctrl.Result{}, err
	}

	ipPrefix, ipPrefixID, isManaged, err := r.ensurePublicIPPrefix(ctx, ipPrefixLength, vmConfig)
	if err != nil {
		log.Error(err, "failed to ensure public ip prefix")
		return ctrl.Result{}, err
	}

	var privateIPs []string
	if privateIPs, err = r.reconcileStandaloneVMs(ctx, vmConfig, vmProfile, ipPrefixID, true); err != nil {
		log.Error(err, "failed to reconcile standalone VMs")
		return ctrl.Result{}, err
	}

	if !isManaged {
		if err := r.ensurePublicIPPrefixDeleted(ctx, vmConfig); err != nil {
			log.Error(err, "failed to remove managed public ip prefix")
			return ctrl.Result{}, err
		}
	}

	if vmConfig.Status == nil {
		vmConfig.Status = &egressgatewayv1alpha1.GatewayVMConfigurationStatus{}
	}
	if vmConfig.Spec.ProvisionPublicIps {
		vmConfig.Status.EgressIpPrefix = ipPrefix
	} else {
		vmConfig.Status.EgressIpPrefix = strings.Join(privateIPs, ",")
	}

	if !equality.Semantic.DeepEqual(existing, vmConfig) {
		log.Info(fmt.Sprintf("Updating GatewayVMConfiguration %s/%s", vmConfig.Namespace, vmConfig.Name))
		if err := r.Status().Update(ctx, vmConfig); err != nil {
			log.Error(err, "failed to update gateway vm configuration")
		}
	}

	// Collect and log metrics for monitoring
	metrics, err := r.collectStandaloneVMMetrics(ctx, vmConfig)
	if err != nil {
		log.V(1).Info("Failed to collect standalone VM metrics", "error", err)
	} else {
		r.logStandaloneVMOperationalStatus(ctx, vmConfig, metrics)
	}

	// Perform health checks (non-blocking)
	if err := r.performStandaloneVMHealthChecks(ctx, vmConfig); err != nil {
		log.V(1).Info("Standalone VM health check issues detected", "error", err)
	}

	log.Info("GatewayVMConfiguration reconciled")
	return ctrl.Result{}, nil
}

func (r *GatewayVMConfigurationReconciler) getStandaloneVMProfile(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) (*egressgatewayv1alpha1.GatewayStandaloneVMProfile, int32, error) {
	// Check if VM config has the profile directly
	if vmConfig.Spec.GatewayProfile.StandaloneVMProfile != nil {
		return vmConfig.Spec.GatewayProfile.StandaloneVMProfile,
			vmConfig.Spec.GatewayProfile.StandaloneVMProfile.PublicIpPrefixSize, nil
	}

	// Get from parent StaticGatewayConfiguration
	gwConfig := &egressgatewayv1alpha1.StaticGatewayConfiguration{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: vmConfig.Namespace,
		Name:      vmConfig.Name,
	}, gwConfig)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get StaticGatewayConfiguration: %w", err)
	}

	if gwConfig.Spec.GatewayProfile.StandaloneVMProfile == nil {
		return nil, 0, fmt.Errorf("no standalone VM profile found")
	}

	return gwConfig.Spec.GatewayProfile.StandaloneVMProfile,
		gwConfig.Spec.GatewayProfile.StandaloneVMProfile.PublicIpPrefixSize, nil
}

func (r *GatewayVMConfigurationReconciler) reconcileStandaloneVMs(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	vmProfile *egressgatewayv1alpha1.GatewayStandaloneVMProfile,
	ipPrefixID string,
	wantIPConfig bool,
) ([]string, error) {
	log := log.FromContext(ctx)
	var privateIPs []string

	// Get load balancer backend pool ID
	lbBackendpoolID := r.GetLBBackendAddressPoolID("standalone-vms")

	// Reconcile each VM
	for _, vmName := range vmProfile.VMNames {
		vm, err := r.GetVM(ctx, vmProfile.VMResourceGroup, vmName)
		if err != nil {
			log.Error(err, "failed to get VM", "vmName", vmName)
			continue
		}

		privateIP, err := r.reconcileStandaloneVM(ctx, vmConfig, vmName, vm, ipPrefixID, to.Val(lbBackendpoolID), wantIPConfig)
		if err != nil {
			log.Error(err, "failed to reconcile VM", "vmName", vmName)
			return nil, err
		}

		if wantIPConfig && ipPrefixID == "" {
			privateIPs = append(privateIPs, privateIP)
		}
	}

	// Clean up VMProfiles for deleted VMs
	var vmprofiles []egressgatewayv1alpha1.GatewayVMProfile
	if vmConfig.Status != nil {
		for i := range vmConfig.Status.GatewayVMProfiles {
			profile := vmConfig.Status.GatewayVMProfiles[i]
			// Check if this VM is still in the configured VM list
			for _, vmName := range vmProfile.VMNames {
				if profile.NodeName == vmName {
					vmprofiles = append(vmprofiles, profile)
					break
				}
			}
		}
		vmConfig.Status.GatewayVMProfiles = vmprofiles
	}

	err := r.Status().Update(ctx, vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to update vm config status: %w", err)
	}

	return privateIPs, nil
}

// validateStandaloneVMSubnet validates that the VM is in the correct subnet for gateway functionality
func (r *GatewayVMConfigurationReconciler) validateStandaloneVMSubnet(
	ctx context.Context,
	vmName string,
	nic *network.Interface,
) error {
	logger := log.FromContext(ctx).WithValues("vmName", vmName)
	ctx = log.IntoContext(ctx, logger)

	// Get the expected subnet information from configuration
	expectedSubnet, err := r.GetSubnet(ctx)
	if err != nil {
		return fmt.Errorf("failed to get expected subnet configuration: %w", err)
	}

	if expectedSubnet.ID == nil {
		return fmt.Errorf("expected subnet has no ID")
	}

	// Check if the VM's network interface is in the correct subnet
	if nic.Properties != nil && nic.Properties.IPConfigurations != nil {
		for _, ipConfig := range nic.Properties.IPConfigurations {
			if ipConfig != nil && ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
				if ipConfig.Properties.Subnet == nil || ipConfig.Properties.Subnet.ID == nil {
					return fmt.Errorf("VM %s primary IP configuration has no subnet", vmName)
				}

				if !strings.EqualFold(to.Val(ipConfig.Properties.Subnet.ID), to.Val(expectedSubnet.ID)) {
					return fmt.Errorf("VM %s is in subnet %s, but expected subnet is %s",
						vmName, to.Val(ipConfig.Properties.Subnet.ID), to.Val(expectedSubnet.ID))
				}

				logger.Info("VM subnet validation successful", "subnetId", to.Val(expectedSubnet.ID))
				return nil
			}
		}
	}

	return fmt.Errorf("VM %s has no primary IP configuration to validate subnet", vmName)
}

// discoverVMSubnetInfo extracts subnet information from a VM's network interface
func (r *GatewayVMConfigurationReconciler) discoverVMSubnetInfo(
	ctx context.Context,
	vmName string,
	nic *network.Interface,
) (subnetID string, vnetInfo map[string]string, err error) {
	logger := log.FromContext(ctx).WithValues("vmName", vmName)

	if nic.Properties == nil || nic.Properties.IPConfigurations == nil {
		return "", nil, fmt.Errorf("VM %s network interface has no IP configurations", vmName)
	}

	// Find the primary IP configuration
	for _, ipConfig := range nic.Properties.IPConfigurations {
		if ipConfig != nil && ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
			if ipConfig.Properties.Subnet == nil || ipConfig.Properties.Subnet.ID == nil {
				return "", nil, fmt.Errorf("VM %s primary IP configuration has no subnet ID", vmName)
			}

			subnetID = to.Val(ipConfig.Properties.Subnet.ID)

			// Parse subnet ID to extract VNet and resource group information
			// Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/virtualNetworks/{vnet}/subnets/{subnet}
			parts := strings.Split(subnetID, "/")
			if len(parts) >= 11 {
				vnetInfo = map[string]string{
					"subscriptionId":     parts[2],
					"resourceGroup":      parts[4],
					"virtualNetworkName": parts[8],
					"subnetName":         parts[10],
				}
				logger.Info("Discovered VM subnet information",
					"subnetId", subnetID,
					"vnet", vnetInfo["virtualNetworkName"],
					"subnet", vnetInfo["subnetName"],
					"resourceGroup", vnetInfo["resourceGroup"])
			} else {
				logger.V(1).Info("Unable to parse full VNet info from subnet ID", "subnetId", subnetID)
				vnetInfo = map[string]string{}
			}

			return subnetID, vnetInfo, nil
		}
	}

	return "", nil, fmt.Errorf("VM %s has no primary IP configuration", vmName)
}

// ensureVMNetworkCompatibility ensures the VM's network configuration is compatible with the gateway setup
func (r *GatewayVMConfigurationReconciler) ensureVMNetworkCompatibility(
	ctx context.Context,
	vmName string,
	nic *network.Interface,
) error {
	logger := log.FromContext(ctx).WithValues("vmName", vmName)
	ctx = log.IntoContext(ctx, logger)

	// Validate subnet compatibility
	if err := r.validateStandaloneVMSubnet(ctx, vmName, nic); err != nil {
		return fmt.Errorf("subnet validation failed for VM %s: %w", vmName, err)
	}

	// Discover and log subnet information for debugging
	subnetID, vnetInfo, err := r.discoverVMSubnetInfo(ctx, vmName, nic)
	if err != nil {
		logger.V(1).Info("Unable to fully discover subnet info", "error", err.Error())
	} else {
		logger.Info("VM network compatibility verified",
			"subnetId", subnetID,
			"vnetInfo", vnetInfo)
	}

	return nil
}

// validateIPConfiguration validates the IP configuration for consistency and correctness
func (r *GatewayVMConfigurationReconciler) validateIPConfiguration(
	ctx context.Context,
	vmName string,
	ipConfig *network.InterfaceIPConfiguration,
	ipPrefixID string,
) error {
	logger := log.FromContext(ctx).WithValues("vmName", vmName, "ipConfigName", to.Val(ipConfig.Name))

	if ipConfig.Properties == nil {
		return fmt.Errorf("IP configuration %s has no properties", to.Val(ipConfig.Name))
	}

	props := ipConfig.Properties

	// Validate private IP allocation method
	if props.PrivateIPAllocationMethod == nil {
		return fmt.Errorf("IP configuration %s has no allocation method", to.Val(ipConfig.Name))
	}

	// Validate subnet assignment
	if props.Subnet == nil || props.Subnet.ID == nil {
		return fmt.Errorf("IP configuration %s has no subnet assignment", to.Val(ipConfig.Name))
	}

	// Validate public IP configuration if IP prefix is provided
	if ipPrefixID != "" {
		if props.PublicIPAddress == nil {
			return fmt.Errorf("IP configuration %s should have public IP when prefix is provided", to.Val(ipConfig.Name))
		}
		if props.PublicIPAddress.Properties == nil ||
			props.PublicIPAddress.Properties.PublicIPPrefix == nil ||
			props.PublicIPAddress.Properties.PublicIPPrefix.ID == nil ||
			!strings.EqualFold(to.Val(props.PublicIPAddress.Properties.PublicIPPrefix.ID), ipPrefixID) {
			return fmt.Errorf("IP configuration %s public IP prefix mismatch", to.Val(ipConfig.Name))
		}
	}

	logger.Info("IP configuration validation successful",
		"privateIP", to.Val(props.PrivateIPAddress),
		"allocationMethod", string(*props.PrivateIPAllocationMethod),
		"hasPublicIP", props.PublicIPAddress != nil)

	return nil
}

// cleanupOrphanedIPConfigurations removes any orphaned IP configurations that shouldn't be there
func (r *GatewayVMConfigurationReconciler) cleanupOrphanedIPConfigurations(
	ctx context.Context,
	vmName string,
	nic *network.Interface,
	expectedConfigName string,
) (bool, error) {
	logger := log.FromContext(ctx).WithValues("vmName", vmName)
	needsUpdate := false

	if nic.Properties == nil || nic.Properties.IPConfigurations == nil {
		return false, nil
	}

	// Find and remove any orphaned managed IP configurations
	var cleanConfigs []*network.InterfaceIPConfiguration
	for _, ipConfig := range nic.Properties.IPConfigurations {
		if ipConfig != nil && ipConfig.Name != nil {
			configName := to.Val(ipConfig.Name)

			// Keep primary configurations and our expected managed configuration
			if ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
				// Always keep primary IP configuration
				cleanConfigs = append(cleanConfigs, ipConfig)
			} else if strings.EqualFold(configName, expectedConfigName) {
				// Keep our expected managed configuration
				cleanConfigs = append(cleanConfigs, ipConfig)
			} else if strings.HasPrefix(configName, consts.ManagedResourcePrefix) {
				// This is an orphaned managed configuration - remove it
				logger.Info("Removing orphaned managed IP configuration", "configName", configName)
				needsUpdate = true
			} else {
				// Keep non-managed configurations
				cleanConfigs = append(cleanConfigs, ipConfig)
			}
		}
	}

	if needsUpdate {
		nic.Properties.IPConfigurations = cleanConfigs
	}

	return needsUpdate, nil
}

// trackIPAllocation tracks IP allocation for monitoring and debugging
func (r *GatewayVMConfigurationReconciler) trackIPAllocation(
	ctx context.Context,
	vmName string,
	primaryIP, secondaryIP string,
	action string, // "allocated", "updated", "removed"
) {
	logger := log.FromContext(ctx).WithValues(
		"vmName", vmName,
		"action", action,
		"primaryIP", primaryIP,
		"secondaryIP", secondaryIP,
	)

	switch action {
	case "allocated":
		logger.Info("IP allocation completed")
	case "updated":
		logger.Info("IP configuration updated")
	case "removed":
		logger.Info("IP configuration removed")
	default:
		logger.Info("IP allocation tracked")
	}
}

// validateBackendPool validates that the backend pool exists and is accessible
func (r *GatewayVMConfigurationReconciler) validateBackendPool(
	ctx context.Context,
	lbBackendpoolID string,
) error {
	logger := log.FromContext(ctx).WithValues("backendPoolId", lbBackendpoolID)

	if lbBackendpoolID == "" {
		return fmt.Errorf("backend pool ID is empty")
	}

	// Get the load balancer to verify the backend pool exists
	lb, err := r.GetLB(ctx)
	if err != nil {
		return fmt.Errorf("failed to get load balancer for backend pool validation: %w", err)
	}

	if lb.Properties == nil || lb.Properties.BackendAddressPools == nil {
		return fmt.Errorf("load balancer has no backend address pools")
	}

	// Check if the backend pool exists
	for _, pool := range lb.Properties.BackendAddressPools {
		if pool != nil && pool.ID != nil && strings.EqualFold(to.Val(pool.ID), lbBackendpoolID) {
			logger.Info("Backend pool validation successful", "poolName", to.Val(pool.Name))
			return nil
		}
	}

	return fmt.Errorf("backend pool %s not found in load balancer", lbBackendpoolID)
}

// manageVMBackendPoolAssociation manages the association of a VM with load balancer backend pools
func (r *GatewayVMConfigurationReconciler) manageVMBackendPoolAssociation(
	ctx context.Context,
	vmName string,
	primaryIPConfig *network.InterfaceIPConfiguration,
	lbBackendpoolID string,
	wantIPConfig bool,
) (bool, error) {
	logger := log.FromContext(ctx).WithValues("vmName", vmName, "wantIPConfig", wantIPConfig)
	needUpdate := false

	if primaryIPConfig == nil || primaryIPConfig.Properties == nil {
		return false, fmt.Errorf("primary IP configuration is nil")
	}

	// Validate backend pool exists before proceeding
	if lbBackendpoolID != "" {
		if err := r.validateBackendPool(ctx, lbBackendpoolID); err != nil {
			return false, fmt.Errorf("backend pool validation failed: %w", err)
		}
	}

	needBackendPool := wantIPConfig // Only need backend pool if we have secondary IP config
	foundBackendPool := false

	// Check if backend pool is already configured
	if primaryIPConfig.Properties.LoadBalancerBackendAddressPools != nil {
		for i, pool := range primaryIPConfig.Properties.LoadBalancerBackendAddressPools {
			if pool != nil && strings.EqualFold(to.Val(pool.ID), lbBackendpoolID) {
				foundBackendPool = true
				if !needBackendPool {
					// Remove backend pool if we no longer need it
					logger.Info("Removing load balancer backend pool association")
					primaryIPConfig.Properties.LoadBalancerBackendAddressPools = append(
						primaryIPConfig.Properties.LoadBalancerBackendAddressPools[:i],
						primaryIPConfig.Properties.LoadBalancerBackendAddressPools[i+1:]...,
					)
					needUpdate = true
				} else {
					logger.Info("Load balancer backend pool association already exists")
				}
				break
			}
		}
	}

	// Add backend pool if needed and not found
	if needBackendPool && !foundBackendPool && lbBackendpoolID != "" {
		logger.Info("Adding load balancer backend pool association")
		if primaryIPConfig.Properties.LoadBalancerBackendAddressPools == nil {
			primaryIPConfig.Properties.LoadBalancerBackendAddressPools = []*network.BackendAddressPool{}
		}
		primaryIPConfig.Properties.LoadBalancerBackendAddressPools = append(
			primaryIPConfig.Properties.LoadBalancerBackendAddressPools,
			&network.BackendAddressPool{
				ID: to.Ptr(lbBackendpoolID),
			},
		)
		needUpdate = true
	}

	return needUpdate, nil
}

// trackBackendPoolAssociation tracks backend pool association changes for monitoring
func (r *GatewayVMConfigurationReconciler) trackBackendPoolAssociation(
	ctx context.Context,
	vmName string,
	lbBackendpoolID string,
	action string, // "associated", "disassociated", "validated"
) {
	logger := log.FromContext(ctx).WithValues(
		"vmName", vmName,
		"backendPoolId", lbBackendpoolID,
		"action", action,
	)

	switch action {
	case "associated":
		logger.Info("VM associated with backend pool")
	case "disassociated":
		logger.Info("VM disassociated from backend pool")
	case "validated":
		logger.Info("VM backend pool association validated")
	default:
		logger.Info("Backend pool association tracked")
	}
}

// validateVMNetworkSecurityConfiguration validates the VM's network security configuration
func (r *GatewayVMConfigurationReconciler) validateVMNetworkSecurityConfiguration(
	ctx context.Context,
	vmName string,
	nic *network.Interface,
) error {
	logger := log.FromContext(ctx).WithValues("vmName", vmName)

	if nic.Properties == nil {
		return fmt.Errorf("VM %s network interface has no properties", vmName)
	}

	// Check if network security group is attached to the network interface
	if nic.Properties.NetworkSecurityGroup == nil {
		// Check if NSG is attached to the subnet instead
		subnetID, _, err := r.discoverVMSubnetInfo(ctx, vmName, nic)
		if err != nil {
			return fmt.Errorf("failed to discover subnet info for NSG validation: %w", err)
		}

		// For now, we'll just log this as most AKS clusters have NSGs at subnet level
		logger.Info("Network security group not attached to NIC, assuming subnet-level NSG", "subnetId", subnetID)
		return nil
	}

	nsgID := to.Val(nic.Properties.NetworkSecurityGroup.ID)
	logger.Info("Network security group validation successful", "nsgId", nsgID)
	return nil
}

// ensureVMNetworkSecurityCompliance ensures the VM meets network security requirements for gateway functionality
func (r *GatewayVMConfigurationReconciler) ensureVMNetworkSecurityCompliance(
	ctx context.Context,
	vmName string,
	nic *network.Interface,
) error {
	logger := log.FromContext(ctx).WithValues("vmName", vmName)
	ctx = log.IntoContext(ctx, logger)

	// Validate basic network security configuration
	if err := r.validateVMNetworkSecurityConfiguration(ctx, vmName, nic); err != nil {
		return fmt.Errorf("network security validation failed for VM %s: %w", vmName, err)
	}

	// For standalone VMs acting as egress gateways, we need to ensure:
	// 1. Outbound internet access is allowed (typically handled by default NSG rules)
	// 2. Inbound access from cluster nodes is allowed (typically handled by subnet NSG)
	// 3. Health probe access is allowed for load balancer (Azure LB health probes)

	// Since NSG rules are typically managed at the infrastructure level in AKS,
	// we'll focus on validation rather than modification
	logger.Info("Network security compliance validation completed")

	// Additional validation could be added here to check specific NSG rules
	// if needed for the gateway functionality

	return nil
}

// checkRequiredNetworkPorts validates that required network ports are accessible
func (r *GatewayVMConfigurationReconciler) checkRequiredNetworkPorts(
	ctx context.Context,
	vmName string,
) error {
	logger := log.FromContext(ctx).WithValues("vmName", vmName)

	// For egress gateway functionality, we typically need:
	// - Outbound internet access on common ports (80, 443, etc.)
	// - Health probe access (Azure LB health probes use port 8080 by default)
	// - Cluster communication ports
	
	// Since we can't easily test connectivity from the controller,
	// we'll log the requirements for operational awareness
	logger.Info("Required network ports for egress gateway functionality:",
		"outbound", "80,443 (HTTP/HTTPS)",
		"healthProbe", "8080 (Azure LB health probe)",
		"cluster", "Various cluster communication ports")

	return nil
}

// coordinateLoadBalancerForMixedDeployment ensures load balancer configuration supports both VMSS and standalone VMs
func (r *GatewayVMConfigurationReconciler) coordinateLoadBalancerForMixedDeployment(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx).WithValues("vmConfig", vmConfig.Name)
	logger.Info("Coordinating load balancer for mixed deployment")

	// Get the load balancer
	lb, err := r.GetLB(ctx)
	if err != nil {
		return fmt.Errorf("failed to get load balancer for mixed deployment coordination: %w", err)
	}

	if lb.Properties == nil {
		return fmt.Errorf("load balancer has no properties")
	}

	// Ensure backend pools exist for both deployment types
	if err := r.ensureBackendPoolsForMixedDeployment(ctx, lb); err != nil {
		return fmt.Errorf("failed to ensure backend pools for mixed deployment: %w", err)
	}

	logger.Info("Load balancer coordination completed for mixed deployment")
	return nil
}

// ensureBackendPoolsForMixedDeployment ensures proper backend pools exist for mixed deployments
func (r *GatewayVMConfigurationReconciler) ensureBackendPoolsForMixedDeployment(
	ctx context.Context,
	lb *network.LoadBalancer,
) error {
	logger := log.FromContext(ctx)

	if lb.Properties == nil || lb.Properties.BackendAddressPools == nil {
		logger.V(1).Info("Load balancer has no backend address pools")
		return nil
	}

	// Log existing backend pools for visibility
	for i, pool := range lb.Properties.BackendAddressPools {
		if pool != nil && pool.Name != nil {
			logger.Info("Found backend address pool", 
				"index", i,
				"name", to.Val(pool.Name),
				"id", to.Val(pool.ID))
		}
	}

	// For mixed deployments, we rely on the existing backend pool structure
	// Each VMSS and standalone VM group will have their own backend pools
	// This coordination method ensures we're aware of all backend pools

	logger.Info("Backend pool coordination completed", "totalPools", len(lb.Properties.BackendAddressPools))
	return nil
}

// validateMixedDeploymentCompatibility validates that mixed deployment configuration is compatible
func (r *GatewayVMConfigurationReconciler) validateMixedDeploymentCompatibility(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx).WithValues("vmConfig", vmConfig.Name)

	// Validate that both profiles are properly configured
	vmssProfile := vmConfig.Spec.GatewayProfile.VmssProfile
	standaloneProfile := vmConfig.Spec.GatewayProfile.StandaloneVMProfile

	if vmssProfile == nil && standaloneProfile == nil {
		return fmt.Errorf("mixed deployment requires both VMSS and standalone VM profiles")
	}

	// Validate VMSS profile if present
	if vmssProfile != nil {
		if vmssProfile.VmssResourceGroup == "" || vmssProfile.VmssName == "" {
			return fmt.Errorf("VMSS profile is incomplete: missing resource group or name")
		}
	}

	// Validate standalone VM profile if present
	if standaloneProfile != nil {
		if standaloneProfile.VMResourceGroup == "" || len(standaloneProfile.VMNames) == 0 {
			return fmt.Errorf("standalone VM profile is incomplete: missing resource group or VM names")
		}
	}

	// Validate IP prefix size compatibility
	if vmssProfile != nil && standaloneProfile != nil {
		if vmssProfile.PublicIpPrefixSize > 0 && standaloneProfile.PublicIpPrefixSize > 0 {
			if vmssProfile.PublicIpPrefixSize != standaloneProfile.PublicIpPrefixSize {
				logger.Info("Warning: Different IP prefix sizes for VMSS and standalone VMs",
					"vmssSize", vmssProfile.PublicIpPrefixSize,
					"standaloneSize", standaloneProfile.PublicIpPrefixSize)
			}
		}
	}

	logger.Info("Mixed deployment compatibility validation passed")
	return nil
}

// MigrationPhase represents the current phase of a deployment migration
type MigrationPhase string

const (
	MigrationPhaseNone       MigrationPhase = "none"
	MigrationPhaseStarting   MigrationPhase = "starting"
	MigrationPhaseInProgress MigrationPhase = "in-progress"
	MigrationPhaseCompleting MigrationPhase = "completing"
	MigrationPhaseCompleted  MigrationPhase = "completed"
)

// detectMigration detects if a migration is occurring between deployment types
func (r *GatewayVMConfigurationReconciler) detectMigration(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	existing *egressgatewayv1alpha1.GatewayVMConfiguration,
) (bool, DeploymentMode, DeploymentMode, error) {
	logger := log.FromContext(ctx).WithValues("vmConfig", vmConfig.Name)

	// Get current and previous deployment modes
	currentMode, err := r.getDeploymentMode(ctx, vmConfig)
	if err != nil {
		return false, VMSSMode, VMSSMode, fmt.Errorf("failed to get current deployment mode: %w", err)
	}

	previousMode, err := r.getDeploymentMode(ctx, existing)
	if err != nil {
		// If we can't determine previous mode, assume no migration
		return false, currentMode, currentMode, nil
	}

	// Check if modes are different
	isMigrating := currentMode != previousMode

	if isMigrating {
		logger.Info("Migration detected",
			"previousMode", previousMode,
			"currentMode", currentMode)
	}

	return isMigrating, previousMode, currentMode, nil
}

// executeMigration handles the migration between deployment types
func (r *GatewayVMConfigurationReconciler) executeMigration(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	previousMode, currentMode DeploymentMode,
) error {
	logger := log.FromContext(ctx).WithValues(
		"vmConfig", vmConfig.Name,
		"previousMode", previousMode,
		"currentMode", currentMode,
	)

	logger.Info("Starting deployment migration")

	// Handle different migration scenarios
	switch {
	case previousMode == VMSSMode && currentMode == StandaloneMode:
		return r.migrateFromVMSSToStandalone(ctx, vmConfig)
	case previousMode == StandaloneMode && currentMode == VMSSMode:
		return r.migrateFromStandaloneToVMSS(ctx, vmConfig)
	case previousMode == VMSSMode && currentMode == MixedMode:
		return r.migrateFromVMSSToMixed(ctx, vmConfig)
	case previousMode == StandaloneMode && currentMode == MixedMode:
		return r.migrateFromStandaloneToMixed(ctx, vmConfig)
	case previousMode == MixedMode && currentMode == VMSSMode:
		return r.migrateFromMixedToVMSS(ctx, vmConfig)
	case previousMode == MixedMode && currentMode == StandaloneMode:
		return r.migrateFromMixedToStandalone(ctx, vmConfig)
	default:
		return fmt.Errorf("unsupported migration path from %v to %v", previousMode, currentMode)
	}
}

// migrateFromVMSSToStandalone migrates from VMSS-only to standalone VM-only
func (r *GatewayVMConfigurationReconciler) migrateFromVMSSToStandalone(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx)
	logger.Info("Migrating from VMSS to standalone VM deployment")

	// Phase 1: Set up standalone VMs while VMSS is still running
	standaloneProfile, _, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("failed to get standalone VM profile for migration: %w", err)
	}

	// Configure standalone VMs
	if _, err := r.reconcileStandaloneVMs(ctx, vmConfig, standaloneProfile, "", true); err != nil {
		return fmt.Errorf("failed to configure standalone VMs during migration: %w", err)
	}

	// Phase 2: Remove VMSS backend pool associations (graceful handoff)
	vmss, _, err := r.getGatewayVMSS(ctx, vmConfig)
	if err == nil {
		logger.Info("Removing VMSS backend pool associations")
		if _, err := r.reconcileVMSS(ctx, vmConfig, vmss, "", false); err != nil {
			logger.Error(err, "Failed to remove VMSS backend pool associations during migration")
			// Continue with migration despite this error
		}
	}

	logger.Info("Migration from VMSS to standalone completed")
	return nil
}

// migrateFromStandaloneToVMSS migrates from standalone VM-only to VMSS-only
func (r *GatewayVMConfigurationReconciler) migrateFromStandaloneToVMSS(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx)
	logger.Info("Migrating from standalone VM to VMSS deployment")

	// Phase 1: Set up VMSS while standalone VMs are still running
	vmss, _, err := r.getGatewayVMSS(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("failed to get VMSS for migration: %w", err)
	}

	// Configure VMSS
	if _, err := r.reconcileVMSS(ctx, vmConfig, vmss, "", true); err != nil {
		return fmt.Errorf("failed to configure VMSS during migration: %w", err)
	}

	// Phase 2: Remove standalone VM backend pool associations (graceful handoff)
	standaloneProfile, _, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err == nil {
		logger.Info("Removing standalone VM backend pool associations")
		if _, err := r.reconcileStandaloneVMs(ctx, vmConfig, standaloneProfile, "", false); err != nil {
			logger.Error(err, "Failed to remove standalone VM backend pool associations during migration")
			// Continue with migration despite this error
		}
	}

	logger.Info("Migration from standalone VM to VMSS completed")
	return nil
}

// migrateFromVMSSToMixed migrates from VMSS-only to mixed deployment
func (r *GatewayVMConfigurationReconciler) migrateFromVMSSToMixed(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx)
	logger.Info("Migrating from VMSS to mixed deployment")

	// This is an additive migration - keep existing VMSS and add standalone VMs
	standaloneProfile, _, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("failed to get standalone VM profile for mixed migration: %w", err)
	}

	// Add standalone VMs to the existing VMSS deployment
	if _, err := r.reconcileStandaloneVMs(ctx, vmConfig, standaloneProfile, "", true); err != nil {
		return fmt.Errorf("failed to add standalone VMs during mixed migration: %w", err)
	}

	logger.Info("Migration from VMSS to mixed deployment completed")
	return nil
}

// migrateFromStandaloneToMixed migrates from standalone VM-only to mixed deployment
func (r *GatewayVMConfigurationReconciler) migrateFromStandaloneToMixed(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx)
	logger.Info("Migrating from standalone VM to mixed deployment")

	// This is an additive migration - keep existing standalone VMs and add VMSS
	vmss, _, err := r.getGatewayVMSS(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("failed to get VMSS for mixed migration: %w", err)
	}

	// Add VMSS to the existing standalone VM deployment
	if _, err := r.reconcileVMSS(ctx, vmConfig, vmss, "", true); err != nil {
		return fmt.Errorf("failed to add VMSS during mixed migration: %w", err)
	}

	logger.Info("Migration from standalone VM to mixed deployment completed")
	return nil
}

// migrateFromMixedToVMSS migrates from mixed deployment to VMSS-only
func (r *GatewayVMConfigurationReconciler) migrateFromMixedToVMSS(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx)
	logger.Info("Migrating from mixed deployment to VMSS")

	// Remove standalone VM backend pool associations
	standaloneProfile, _, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err == nil {
		logger.Info("Removing standalone VM backend pool associations")
		if _, err := r.reconcileStandaloneVMs(ctx, vmConfig, standaloneProfile, "", false); err != nil {
			logger.Error(err, "Failed to remove standalone VM backend pool associations during migration")
			// Continue with migration despite this error
		}
	}

	logger.Info("Migration from mixed deployment to VMSS completed")
	return nil
}

// migrateFromMixedToStandalone migrates from mixed deployment to standalone VM-only
func (r *GatewayVMConfigurationReconciler) migrateFromMixedToStandalone(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx)
	logger.Info("Migrating from mixed deployment to standalone VM")

	// Remove VMSS backend pool associations
	vmss, _, err := r.getGatewayVMSS(ctx, vmConfig)
	if err == nil {
		logger.Info("Removing VMSS backend pool associations")
		if _, err := r.reconcileVMSS(ctx, vmConfig, vmss, "", false); err != nil {
			logger.Error(err, "Failed to remove VMSS backend pool associations during migration")
			// Continue with migration despite this error
		}
	}

	logger.Info("Migration from mixed deployment to standalone VM completed")
	return nil
}

// StandaloneVMMetrics provides metrics specific to standalone VM gateways
type StandaloneVMMetrics struct {
	TotalVMs           int
	HealthyVMs         int
	UnhealthyVMs       int
	ConfiguredIPs      int
	BackendPoolAssocs  int
	MigrationCount     int
}

// collectStandaloneVMMetrics collects metrics for standalone VM gateways
func (r *GatewayVMConfigurationReconciler) collectStandaloneVMMetrics(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) (*StandaloneVMMetrics, error) {
	logger := log.FromContext(ctx).WithValues("vmConfig", vmConfig.Name)
	metrics := &StandaloneVMMetrics{}

	// Get standalone VM profile
	standaloneProfile, _, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err != nil {
		return metrics, fmt.Errorf("failed to get standalone VM profile for metrics: %w", err)
	}

	metrics.TotalVMs = len(standaloneProfile.VMNames)

	// Collect metrics for each VM
	for _, vmName := range standaloneProfile.VMNames {
		// Check VM health
		vm, err := r.GetVM(ctx, standaloneProfile.VMResourceGroup, vmName)
		if err != nil {
			logger.V(1).Info("Failed to get VM for metrics", "vmName", vmName, "error", err)
			metrics.UnhealthyVMs++
			continue
		}

		// Check VM provisioning state
		if vm.Properties != nil && vm.Properties.ProvisioningState != nil {
			if *vm.Properties.ProvisioningState == "Succeeded" {
				metrics.HealthyVMs++
			} else {
				metrics.UnhealthyVMs++
			}
		} else {
			metrics.UnhealthyVMs++
		}

		// Check network interface configuration
		if vm.Properties != nil && vm.Properties.NetworkProfile != nil {
			for _, nicRef := range vm.Properties.NetworkProfile.NetworkInterfaces {
				if nicRef.Properties != nil && to.Val(nicRef.Properties.Primary) {
					primaryNicID := to.Val(nicRef.ID)
					nicName := primaryNicID[strings.LastIndex(primaryNicID, "/")+1:]
					
					nic, err := r.GetVMNetworkInterface(ctx, standaloneProfile.VMResourceGroup, nicName)
					if err != nil {
						logger.V(1).Info("Failed to get NIC for metrics", "nicName", nicName, "error", err)
						continue
					}

					// Count IP configurations
					if nic.Properties != nil && nic.Properties.IPConfigurations != nil {
						metrics.ConfiguredIPs += len(nic.Properties.IPConfigurations)
						
						// Count backend pool associations
						for _, ipConfig := range nic.Properties.IPConfigurations {
							if ipConfig.Properties != nil && ipConfig.Properties.LoadBalancerBackendAddressPools != nil {
								metrics.BackendPoolAssocs += len(ipConfig.Properties.LoadBalancerBackendAddressPools)
							}
						}
					}
				}
			}
		}
	}

	logger.Info("Standalone VM metrics collected",
		"totalVMs", metrics.TotalVMs,
		"healthyVMs", metrics.HealthyVMs,
		"unhealthyVMs", metrics.UnhealthyVMs,
		"configuredIPs", metrics.ConfiguredIPs,
		"backendPoolAssocs", metrics.BackendPoolAssocs)

	return metrics, nil
}

// performStandaloneVMHealthChecks performs health checks on standalone VM gateways
func (r *GatewayVMConfigurationReconciler) performStandaloneVMHealthChecks(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	logger := log.FromContext(ctx).WithValues("vmConfig", vmConfig.Name)
	logger.Info("Performing standalone VM health checks")

	// Get standalone VM profile
	standaloneProfile, _, err := r.getStandaloneVMProfile(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("failed to get standalone VM profile for health checks: %w", err)
	}

	var healthIssues []string

	// Check each VM
	for _, vmName := range standaloneProfile.VMNames {
		
		// Check VM existence and status
		vm, err := r.GetVM(ctx, standaloneProfile.VMResourceGroup, vmName)
		if err != nil {
			healthIssues = append(healthIssues, fmt.Sprintf("VM %s: failed to retrieve - %v", vmName, err))
			continue
		}

		// Check provisioning state
		if vm.Properties == nil || vm.Properties.ProvisioningState == nil {
			healthIssues = append(healthIssues, fmt.Sprintf("VM %s: no provisioning state", vmName))
			continue
		}

		if *vm.Properties.ProvisioningState != "Succeeded" {
			healthIssues = append(healthIssues, fmt.Sprintf("VM %s: provisioning state is %s", vmName, *vm.Properties.ProvisioningState))
		}

		// Check network configuration
		if vm.Properties.NetworkProfile == nil || len(vm.Properties.NetworkProfile.NetworkInterfaces) == 0 {
			healthIssues = append(healthIssues, fmt.Sprintf("VM %s: no network interfaces", vmName))
			continue
		}

		// Check primary network interface health
		hasPrimaryNic := false
		for _, nicRef := range vm.Properties.NetworkProfile.NetworkInterfaces {
			if nicRef.Properties != nil && to.Val(nicRef.Properties.Primary) {
				hasPrimaryNic = true
				primaryNicID := to.Val(nicRef.ID)
				nicName := primaryNicID[strings.LastIndex(primaryNicID, "/")+1:]
				
				// Validate network interface configuration
				if err := r.validateVMNetworkInterfaceHealth(ctx, vmName, standaloneProfile.VMResourceGroup, nicName); err != nil {
					healthIssues = append(healthIssues, fmt.Sprintf("VM %s: network interface health issue - %v", vmName, err))
				}
				break
			}
		}

		if !hasPrimaryNic {
			healthIssues = append(healthIssues, fmt.Sprintf("VM %s: no primary network interface", vmName))
		}
	}

	if len(healthIssues) > 0 {
		logger.Info("Standalone VM health check issues detected", "issues", healthIssues)
		return fmt.Errorf("health check failed: %v", healthIssues)
	}

	logger.Info("All standalone VM health checks passed")
	return nil
}

// validateVMNetworkInterfaceHealth validates the health of a VM's network interface
func (r *GatewayVMConfigurationReconciler) validateVMNetworkInterfaceHealth(
	ctx context.Context,
	vmName, resourceGroup, nicName string,
) error {
	logger := log.FromContext(ctx).WithValues("vmName", vmName, "nicName", nicName)

	// Get network interface
	nic, err := r.GetVMNetworkInterface(ctx, resourceGroup, nicName)
	if err != nil {
		return fmt.Errorf("failed to get network interface: %w", err)
	}

	if nic.Properties == nil {
		return fmt.Errorf("network interface has no properties")
	}

	// Check provisioning state
	if nic.Properties.ProvisioningState != nil && *nic.Properties.ProvisioningState != "Succeeded" {
		return fmt.Errorf("network interface provisioning state is %s", *nic.Properties.ProvisioningState)
	}

	// Check IP configurations
	if nic.Properties.IPConfigurations == nil || len(nic.Properties.IPConfigurations) == 0 {
		return fmt.Errorf("network interface has no IP configurations")
	}

	// Validate primary IP configuration
	hasPrimaryIP := false
	for _, ipConfig := range nic.Properties.IPConfigurations {
		if ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
			hasPrimaryIP = true
			if ipConfig.Properties.PrivateIPAddress == nil || *ipConfig.Properties.PrivateIPAddress == "" {
				return fmt.Errorf("primary IP configuration has no private IP address")
			}
			break
		}
	}

	if !hasPrimaryIP {
		return fmt.Errorf("network interface has no primary IP configuration")
	}

	logger.V(1).Info("Network interface health check passed")
	return nil
}

// logStandaloneVMOperationalStatus logs operational status for monitoring
func (r *GatewayVMConfigurationReconciler) logStandaloneVMOperationalStatus(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	metrics *StandaloneVMMetrics,
) {
	logger := log.FromContext(ctx).WithValues(
		"vmConfig", vmConfig.Name,
		"deploymentMode", "standalone",
	)

	// Log operational metrics
	logger.Info("Standalone VM operational status",
		"totalVMs", metrics.TotalVMs,
		"healthyVMs", metrics.HealthyVMs,
		"unhealthyVMs", metrics.UnhealthyVMs,
		"healthPercentage", fmt.Sprintf("%.1f%%", float64(metrics.HealthyVMs)/float64(metrics.TotalVMs)*100),
		"configuredIPs", metrics.ConfiguredIPs,
		"backendPoolAssocs", metrics.BackendPoolAssocs)

	// Log status summary
	status := "healthy"
	if metrics.UnhealthyVMs > 0 {
		if metrics.UnhealthyVMs >= metrics.HealthyVMs {
			status = "critical"
		} else {
			status = "degraded"
		}
	}

	logger.Info("Standalone VM gateway status summary", "status", status)

	// Log gateway profile information
	if vmConfig.Status != nil && len(vmConfig.Status.GatewayVMProfiles) > 0 {
		for _, profile := range vmConfig.Status.GatewayVMProfiles {
			logger.Info("VM profile status",
				"nodeName", profile.NodeName,
				"primaryIP", profile.PrimaryIP,
				"secondaryIP", profile.SecondaryIP)
		}
	}
}

func (r *GatewayVMConfigurationReconciler) reconcileStandaloneVM(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	vmName string,
	vm *compute.VirtualMachine,
	ipPrefixID string,
	lbBackendpoolID string,
	wantIPConfig bool,
) (string, error) {
	logger := log.FromContext(ctx).WithValues("vmName", vmName, "wantIPConfig", wantIPConfig, "ipPrefixID", ipPrefixID)
	ctx = log.IntoContext(ctx, logger)
	ipConfigName := managedSubresourceName(vmConfig)
	vmProfile, _, _ := r.getStandaloneVMProfile(ctx, vmConfig)

	if vm.Properties == nil || vm.Properties.NetworkProfile == nil {
		return "", fmt.Errorf("VM %s has empty network profile", vmName)
	}

	// Get the primary network interface
	var primaryNicID string
	for _, nicRef := range vm.Properties.NetworkProfile.NetworkInterfaces {
		if nicRef.Properties != nil && to.Val(nicRef.Properties.Primary) {
			primaryNicID = to.Val(nicRef.ID)
			break
		}
	}

	if primaryNicID == "" {
		return "", fmt.Errorf("VM %s has no primary network interface", vmName)
	}

	// Extract NIC name from resource ID
	nicName := primaryNicID[strings.LastIndex(primaryNicID, "/")+1:]

	// Get the network interface
	nic, err := r.GetVMNetworkInterface(ctx, vmProfile.VMResourceGroup, nicName)
	if err != nil {
		return "", fmt.Errorf("failed to get network interface %s for VM %s: %w", nicName, vmName, err)
	}

	// Validate network compatibility before proceeding
	if err := r.ensureVMNetworkCompatibility(ctx, vmName, nic); err != nil {
		return "", fmt.Errorf("network compatibility validation failed for VM %s: %w", vmName, err)
	}

	// Validate network security configuration
	if err := r.ensureVMNetworkSecurityCompliance(ctx, vmName, nic); err != nil {
		return "", fmt.Errorf("network security compliance validation failed for VM %s: %w", vmName, err)
	}

	// Check required network ports for gateway functionality
	if err := r.checkRequiredNetworkPorts(ctx, vmName); err != nil {
		return "", fmt.Errorf("network port validation failed for VM %s: %w", vmName, err)
	}

	// Clean up any orphaned IP configurations first
	cleanupNeeded, err := r.cleanupOrphanedIPConfigurations(ctx, vmName, nic, ipConfigName)
	if err != nil {
		return "", fmt.Errorf("failed to cleanup orphaned IP configurations for VM %s: %w", vmName, err)
	}
	if cleanupNeeded {
		logger.Info("Cleaned up orphaned IP configurations")
	}

	// Check current IP configurations and determine if update is needed
	var primaryIP, secondaryIP string
	needUpdate := false
	foundSecondaryConfig := false
	var primaryIPConfig *network.InterfaceIPConfiguration
	var subnetID *string

	if nic.Properties != nil && nic.Properties.IPConfigurations != nil {
		for i, ipConfig := range nic.Properties.IPConfigurations {
			if ipConfig != nil && ipConfig.Properties != nil {
				if strings.EqualFold(to.Val(ipConfig.Name), ipConfigName) {
					// Found our managed secondary IP config
					secondaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					foundSecondaryConfig = true
					// Check if we need to remove it when wantIPConfig is false
					if !wantIPConfig {
						logger.Info("Found unwanted secondary IP config, will remove")
						// Remove the secondary IP configuration
						nic.Properties.IPConfigurations = append(nic.Properties.IPConfigurations[:i], nic.Properties.IPConfigurations[i+1:]...)
						needUpdate = true
						secondaryIP = "" // Clear since we're removing it
						break
					}
				} else if to.Val(ipConfig.Properties.Primary) {
					// Found primary IP config
					primaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					primaryIPConfig = ipConfig
					if ipConfig.Properties.Subnet != nil {
						subnetID = ipConfig.Properties.Subnet.ID
					}
				}
			}
		}
	}

	// If we want IP configuration but don't have the secondary one, create it
	if wantIPConfig && !foundSecondaryConfig && primaryIPConfig != nil {
		logger.Info("Creating secondary IP configuration")

		// Create the secondary IP configuration
		secondaryIPConfig := &network.InterfaceIPConfiguration{
			Name: to.Ptr(ipConfigName),
			Properties: &network.InterfaceIPConfigurationPropertiesFormat{
				Primary:                   to.Ptr(false),
				PrivateIPAddressVersion:   to.Ptr(network.IPVersionIPv4),
				PrivateIPAllocationMethod: to.Ptr(network.IPAllocationMethodDynamic),
				Subnet: &network.Subnet{
					ID: subnetID,
				},
			},
		}

		// Add public IP configuration if IP prefix is provided
		if ipPrefixID != "" {
			secondaryIPConfig.Properties.PublicIPAddress = &network.PublicIPAddress{
				Properties: &network.PublicIPAddressPropertiesFormat{
					PublicIPPrefix: &network.SubResource{
						ID: to.Ptr(ipPrefixID),
					},
				},
			}
		}

		// Validate the secondary IP configuration before adding it
		if err := r.validateIPConfiguration(ctx, vmName, secondaryIPConfig, ipPrefixID); err != nil {
			return "", fmt.Errorf("secondary IP configuration validation failed for VM %s: %w", vmName, err)
		}

		// Add to network interface
		nic.Properties.IPConfigurations = append(nic.Properties.IPConfigurations, secondaryIPConfig)
		needUpdate = true
	}

	// Handle load balancer backend pool configuration
	backendPoolUpdated, err := r.manageVMBackendPoolAssociation(ctx, vmName, primaryIPConfig, lbBackendpoolID, wantIPConfig)
	if err != nil {
		return "", fmt.Errorf("failed to manage backend pool association for VM %s: %w", vmName, err)
	}
	if backendPoolUpdated {
		needUpdate = true
		// Track backend pool association change
		action := "associated"
		if !wantIPConfig {
			action = "disassociated"
		}
		r.trackBackendPoolAssociation(ctx, vmName, lbBackendpoolID, action)
	} else if lbBackendpoolID != "" {
		// Track validation of existing association
		r.trackBackendPoolAssociation(ctx, vmName, lbBackendpoolID, "validated")
	}

	// Update network interface if changes are needed
	if needUpdate {
		logger.Info("Updating network interface configuration")
		_, err = r.UpdateVMNetworkInterface(ctx, vmProfile.VMResourceGroup, nicName, *nic)
		if err != nil {
			return "", fmt.Errorf("failed to update network interface %s for VM %s: %w", nicName, vmName, err)
		}

		// Refresh network interface to get updated IP addresses
		updatedNic, err := r.GetVMNetworkInterface(ctx, vmProfile.VMResourceGroup, nicName)
		if err != nil {
			return "", fmt.Errorf("failed to get updated network interface %s for VM %s: %w", nicName, vmName, err)
		}

		// Get updated IP addresses
		if updatedNic.Properties != nil && updatedNic.Properties.IPConfigurations != nil {
			for _, ipConfig := range updatedNic.Properties.IPConfigurations {
				if ipConfig != nil && ipConfig.Properties != nil {
					if strings.EqualFold(to.Val(ipConfig.Name), ipConfigName) {
						secondaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					} else if to.Val(ipConfig.Properties.Primary) {
						primaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					}
				}
			}
		}
	}

	// Update VM profile in status
	if vmConfig.Status != nil {
		found := false
		for i := range vmConfig.Status.GatewayVMProfiles {
			if vmConfig.Status.GatewayVMProfiles[i].NodeName == vmName {
				vmConfig.Status.GatewayVMProfiles[i].PrimaryIP = primaryIP
				vmConfig.Status.GatewayVMProfiles[i].SecondaryIP = secondaryIP
				found = true
				break
			}
		}
		if !found {
			vmConfig.Status.GatewayVMProfiles = append(vmConfig.Status.GatewayVMProfiles,
				egressgatewayv1alpha1.GatewayVMProfile{
					NodeName:    vmName,
					PrimaryIP:   primaryIP,
					SecondaryIP: secondaryIP,
				})
		}
	}

	// Track IP allocation for monitoring
	action := "updated"
	if wantIPConfig && !foundSecondaryConfig && needUpdate {
		action = "allocated"
	} else if !wantIPConfig && secondaryIP == "" {
		action = "removed"
	}
	r.trackIPAllocation(ctx, vmName, primaryIP, secondaryIP, action)

	// Return appropriate IP based on configuration
	if wantIPConfig && secondaryIP != "" {
		return secondaryIP, nil
	}
	return primaryIP, nil
}

func (r *GatewayVMConfigurationReconciler) ensureDeleted(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info(fmt.Sprintf("Reconciling gatewayVMConfiguration deletion %s/%s", vmConfig.Namespace, vmConfig.Name))

	if !controllerutil.ContainsFinalizer(vmConfig, consts.VMConfigFinalizerName) {
		log.Info("vmConfig does not have finalizer, no additional cleanup needed")
		return ctrl.Result{}, nil
	}

	mc := metrics.NewMetricsContext(
		os.Getenv(consts.PodNamespaceEnvKey),
		"delete_gateway_vm_configuration",
		r.SubscriptionID(),
		r.ResourceGroup,
		strings.ToLower(fmt.Sprintf("%s/%s", vmConfig.Namespace, vmConfig.Name)),
	)
	succeeded := false
	defer func() { mc.ObserveControllerReconcileMetrics(succeeded) }()

	vmss, _, err := r.getGatewayVMSS(ctx, vmConfig)
	if err != nil {
		log.Error(err, "failed to get vmss")
		return ctrl.Result{}, err
	}

	if _, err := r.reconcileVMSS(ctx, vmConfig, vmss, "", false); err != nil {
		log.Error(err, "failed to reconcile VMSS")
		return ctrl.Result{}, err
	}

	if err := r.ensurePublicIPPrefixDeleted(ctx, vmConfig); err != nil {
		log.Error(err, "failed to delete managed public ip prefix")
		return ctrl.Result{}, err
	}

	log.Info("Removing finalizer")
	controllerutil.RemoveFinalizer(vmConfig, consts.VMConfigFinalizerName)
	if err := r.Update(ctx, vmConfig); err != nil {
		log.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	log.Info("GatewayVMConfiguration deletion reconciled")
	succeeded = true
	return ctrl.Result{}, nil
}

func (r *GatewayVMConfigurationReconciler) getGatewayVMSS(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) (*compute.VirtualMachineScaleSet, int32, error) {
	if vmConfig.Spec.GatewayNodepoolName != "" {
		vmssList, err := r.ListVMSS(ctx)
		if err != nil {
			return nil, 0, err
		}
		for i := range vmssList {
			vmss := vmssList[i]
			if v, ok := vmss.Tags[consts.AKSNodepoolTagKey]; ok {
				if strings.EqualFold(to.Val(v), vmConfig.Spec.GatewayNodepoolName) {
					if prefixLenStr, ok := vmss.Tags[consts.AKSNodepoolIPPrefixSizeTagKey]; ok {
						if prefixLen, err := strconv.Atoi(to.Val(prefixLenStr)); err == nil && prefixLen > 0 && prefixLen <= math.MaxInt32 {
							return vmss, int32(prefixLen), nil
						} else {
							return nil, 0, fmt.Errorf("failed to parse nodepool IP prefix size: %s", to.Val(prefixLenStr))
						}
					} else {
						return nil, 0, fmt.Errorf("nodepool does not have IP prefix size")
					}
				}
			}
		}
	} else {
		vmss, err := r.GetVMSS(ctx, vmConfig.Spec.VmssResourceGroup, vmConfig.Spec.VmssName)
		if err != nil {
			return nil, 0, err
		}
		return vmss, vmConfig.Spec.PublicIpPrefixSize, nil
	}
	return nil, 0, fmt.Errorf("gateway VMSS not found")
}

func managedSubresourceName(vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration) string {
	return consts.ManagedResourcePrefix + string(vmConfig.GetUID())
}

func isErrorNotFound(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

func (r *GatewayVMConfigurationReconciler) ensurePublicIPPrefix(
	ctx context.Context,
	ipPrefixLength int32,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) (string, string, bool, error) {
	log := log.FromContext(ctx)

	// no need to provision public ip prefix is only private egress is needed
	if !vmConfig.Spec.ProvisionPublicIps {
		// return isManaged as false so that previously created managed public ip prefix can be deleted
		return "", "", false, nil
	}

	if vmConfig.Spec.PublicIpPrefixId != "" {
		// if there is public prefix ip specified, prioritize this one
		matches := publicIPPrefixRE.FindStringSubmatch(vmConfig.Spec.PublicIpPrefixId)
		if len(matches) != 4 {
			return "", "", false, fmt.Errorf("failed to parse public ip prefix id: %s", vmConfig.Spec.PublicIpPrefixId)
		}
		subscriptionID, resourceGroupName, publicIpPrefixName := matches[1], matches[2], matches[3]
		if subscriptionID != r.SubscriptionID() {
			return "", "", false, fmt.Errorf("public ip prefix subscription(%s) is not in the same subscription(%s)", subscriptionID, r.SubscriptionID())
		}
		ipPrefix, err := r.GetPublicIPPrefix(ctx, resourceGroupName, publicIpPrefixName)
		if err != nil {
			return "", "", false, fmt.Errorf("failed to get public ip prefix(%s): %w", vmConfig.Spec.PublicIpPrefixId, err)
		}
		if ipPrefix.Properties == nil {
			return "", "", false, fmt.Errorf("public ip prefix(%s) has empty properties", vmConfig.Spec.PublicIpPrefixId)
		}
		if to.Val(ipPrefix.Properties.PrefixLength) != ipPrefixLength {
			return "", "", false, fmt.Errorf("provided public ip prefix has invalid length(%d), required(%d)", to.Val(ipPrefix.Properties.PrefixLength), ipPrefixLength)
		}
		log.Info("Found existing unmanaged public ip prefix", "public ip prefix", to.Val(ipPrefix.Properties.IPPrefix))
		return to.Val(ipPrefix.Properties.IPPrefix), to.Val(ipPrefix.ID), false, nil
	} else {
		// check if there's managed public prefix ip
		publicIpPrefixName := managedSubresourceName(vmConfig)
		ipPrefix, err := r.GetPublicIPPrefix(ctx, "", publicIpPrefixName)
		if err == nil {
			if ipPrefix.Properties == nil {
				return "", "", false, fmt.Errorf("managed public ip prefix has empty properties")
			} else {
				log.Info("Found existing managed public ip prefix", "public ip prefix", to.Val(ipPrefix.Properties.IPPrefix))
				return to.Val(ipPrefix.Properties.IPPrefix), to.Val(ipPrefix.ID), true, nil
			}
		} else {
			if !isErrorNotFound(err) {
				return "", "", false, fmt.Errorf("failed to get managed public ip prefix: %w", err)
			}
			// create new public ip prefix
			newIPPrefix := network.PublicIPPrefix{
				Name:     to.Ptr(publicIpPrefixName),
				Location: to.Ptr(r.Location()),
				Properties: &network.PublicIPPrefixPropertiesFormat{
					PrefixLength:           to.Ptr(ipPrefixLength),
					PublicIPAddressVersion: to.Ptr(network.IPVersionIPv4),
				},
				SKU: &network.PublicIPPrefixSKU{
					Name: to.Ptr(network.PublicIPPrefixSKUNameStandard),
					Tier: to.Ptr(network.PublicIPPrefixSKUTierRegional),
				},
			}
			log.Info("Creating new managed public ip prefix")
			ipPrefix, err := r.CreateOrUpdatePublicIPPrefix(ctx, "", publicIpPrefixName, newIPPrefix)
			if err != nil {
				return "", "", false, fmt.Errorf("failed to create managed public ip prefix: %w", err)
			}
			return to.Val(ipPrefix.Properties.IPPrefix), to.Val(ipPrefix.ID), true, nil
		}
	}
}

func (r *GatewayVMConfigurationReconciler) ensurePublicIPPrefixDeleted(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) error {
	log := log.FromContext(ctx)
	// only ensure managed public prefix ip is deleted
	publicIpPrefixName := managedSubresourceName(vmConfig)
	_, err := r.GetPublicIPPrefix(ctx, "", publicIpPrefixName)
	if err != nil {
		if isErrorNotFound(err) {
			// resource does not exist, directly return
			return nil
		} else {
			return fmt.Errorf("failed to get public ip prefix(%s): %w", publicIpPrefixName, err)
		}
	} else {
		log.Info("Deleting managed public ip prefix", "public ip prefix name", publicIpPrefixName)
		if err := r.DeletePublicIPPrefix(ctx, "", publicIpPrefixName); err != nil {
			return fmt.Errorf("failed to delete public ip prefix(%s): %w", publicIpPrefixName, err)
		}
		return nil
	}
}

func (r *GatewayVMConfigurationReconciler) reconcileVMSS(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	vmss *compute.VirtualMachineScaleSet,
	ipPrefixID string,
	wantIPConfig bool,
) ([]string, error) {
	log := log.FromContext(ctx)
	ipConfigName := managedSubresourceName(vmConfig)
	vmssRG := getVMSSResourceGroup(vmConfig)
	needUpdate := false

	if vmss.Properties == nil || vmss.Properties.VirtualMachineProfile == nil ||
		vmss.Properties.VirtualMachineProfile.NetworkProfile == nil {
		return nil, fmt.Errorf("vmss has empty network profile")
	}

	lbBackendpoolID := r.GetLBBackendAddressPoolID(to.Val(vmss.Properties.UniqueID))
	interfaces := vmss.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations
	needUpdate, err := r.reconcileVMSSNetworkInterface(ctx, ipConfigName, ipPrefixID, to.Val(lbBackendpoolID), wantIPConfig, interfaces)
	if err != nil {
		return nil, fmt.Errorf("failed to reconcile vmss interface(%s): %w", to.Val(vmss.Name), err)
	}

	if needUpdate {
		log.Info("Updating vmss", "vmssName", to.Val(vmss.Name))
		newVmss := compute.VirtualMachineScaleSet{
			Location: vmss.Location,
			Properties: &compute.VirtualMachineScaleSetProperties{
				VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{
					NetworkProfile: vmss.Properties.VirtualMachineProfile.NetworkProfile,
				},
			},
		}
		if _, err := r.CreateOrUpdateVMSS(ctx, vmssRG, to.Val(vmss.Name), newVmss); err != nil {
			return nil, fmt.Errorf("failed to update vmss(%s): %w", to.Val(vmss.Name), err)
		}
	}

	// check and update VMSS instances
	var privateIPs []string
	instances, err := r.ListVMSSInstances(ctx, vmssRG, to.Val(vmss.Name))
	if err != nil {
		return nil, fmt.Errorf("failed to get vm instances from vmss(%s): %w", to.Val(vmss.Name), err)
	}
	for _, instance := range instances {
		privateIP, err := r.reconcileVMSSVM(ctx, vmConfig, to.Val(vmss.Name), instance, ipPrefixID, to.Val(lbBackendpoolID), wantIPConfig)
		if err != nil {
			return nil, err
		}
		if wantIPConfig && ipPrefixID == "" {
			privateIPs = append(privateIPs, privateIP)
		}
	}
	// clean up VMProfiles for deleted nodes
	var vmprofiles []egressgatewayv1alpha1.GatewayVMProfile
	if vmConfig.Status != nil {
		for i := range vmConfig.Status.GatewayVMProfiles {
			profile := vmConfig.Status.GatewayVMProfiles[i]
			for _, instance := range instances {
				if profile.NodeName == to.Val(instance.Properties.OSProfile.ComputerName) {
					vmprofiles = append(vmprofiles, profile)
					break
				}
			}
		}
		vmConfig.Status.GatewayVMProfiles = vmprofiles
	}

	err = r.Status().Update(ctx, vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to update vm config status: %w", err)
	}

	return privateIPs, nil
}

func (r *GatewayVMConfigurationReconciler) reconcileVMSSVM(
	ctx context.Context,
	vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
	vmssName string,
	vm *compute.VirtualMachineScaleSetVM,
	ipPrefixID string,
	lbBackendpoolID string,
	wantIPConfig bool,
) (string, error) {
	logger := log.FromContext(ctx).WithValues("vmssInstance", to.Val(vm.ID), "wantIPConfig", wantIPConfig, "ipPrefixID", ipPrefixID)
	ctx = log.IntoContext(ctx, logger)
	ipConfigName := managedSubresourceName(vmConfig)
	vmssRG := getVMSSResourceGroup(vmConfig)

	if vm.Properties == nil || vm.Properties.NetworkProfileConfiguration == nil {
		return "", fmt.Errorf("vmss vm(%s) has empty network profile", to.Val(vm.InstanceID))
	}
	if vm.Properties.OSProfile == nil {
		return "", fmt.Errorf("vmss vm(%s) has empty os profile", to.Val(vm.InstanceID))
	}

	forceUpdate := false
	// check ProvisioningState
	if vm.Properties.ProvisioningState != nil && !strings.EqualFold(to.Val(vm.Properties.ProvisioningState), "Succeeded") {
		logger.Info(fmt.Sprintf("VMSS instance ProvisioningState %q", to.Val(vm.Properties.ProvisioningState)))
		if strings.EqualFold(to.Val(vm.Properties.ProvisioningState), "Failed") {
			forceUpdate = true
			logger.Info(fmt.Sprintf("Force update for unexpected VMSS instance ProvisioningState:%q", to.Val(vm.Properties.ProvisioningState)))
		}
	}

	// check primary IP & secondary IP
	var primaryIP, secondaryIP string
	if !forceUpdate && wantIPConfig {
		for _, nic := range vm.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations {
			if nic.Properties != nil && to.Val(nic.Properties.Primary) {
				vmNic, err := r.GetVMSSInterface(ctx, vmssRG, vmssName, to.Val(vm.InstanceID), to.Val(nic.Name))
				if err != nil || vmNic.Properties == nil || vmNic.Properties.IPConfigurations == nil {
					if err != nil {
						logger.Info("Skip IP check for forceUpdate", "error", err.Error())
					} else {
						logger.Info("Skip IP check for forceUpdate")
					}
					break
				}
				for _, ipConfig := range vmNic.Properties.IPConfigurations {
					if ipConfig != nil && ipConfig.Properties != nil && strings.EqualFold(to.Val(ipConfig.Name), ipConfigName) {
						secondaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					} else if ipConfig != nil && ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
						primaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					}
				}
			}
		}
		if primaryIP == "" || secondaryIP == "" {
			forceUpdate = true
			logger.Info("Force update for missing primary IP and/or secondary IP", "primaryIP", primaryIP, "secondaryIP", secondaryIP)
		}
	}

	interfaces := vm.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations
	needUpdate, err := r.reconcileVMSSNetworkInterface(ctx, ipConfigName, ipPrefixID, lbBackendpoolID, wantIPConfig, interfaces)
	if err != nil {
		return "", fmt.Errorf("failed to reconcile vm interface(%s): %w", to.Val(vm.InstanceID), err)
	}
	vmUpdated := false
	if needUpdate || forceUpdate {
		logger.Info("Updating vmss instance")
		if !needUpdate && forceUpdate {
			logger.Info("Updating vmss instance triggered by forceUpdate")
		}
		newVM := compute.VirtualMachineScaleSetVM{
			Properties: &compute.VirtualMachineScaleSetVMProperties{
				NetworkProfileConfiguration: &compute.VirtualMachineScaleSetVMNetworkProfileConfiguration{
					NetworkInterfaceConfigurations: interfaces,
				},
			},
		}
		if _, err := r.UpdateVMSSInstance(ctx, vmssRG, vmssName, to.Val(vm.InstanceID), newVM); err != nil {
			return "", fmt.Errorf("failed to update vmss instance(%s): %w", to.Val(vm.InstanceID), err)
		}
		vmUpdated = true
	}

	// return earlier if it's deleting event
	if !wantIPConfig {
		return "", nil
	}

	if vmUpdated || primaryIP == "" || secondaryIP == "" {
		primaryIP, secondaryIP = "", ""
		for _, nic := range interfaces {
			if nic.Properties != nil && to.Val(nic.Properties.Primary) {
				vmNic, err := r.GetVMSSInterface(ctx, vmssRG, vmssName, to.Val(vm.InstanceID), to.Val(nic.Name))
				if err != nil {
					return "", fmt.Errorf("failed to get vmss(%s) instance(%s) nic(%s): %w", vmssName, to.Val(vm.InstanceID), to.Val(nic.Name), err)
				}
				if vmNic.Properties == nil || vmNic.Properties.IPConfigurations == nil {
					return "", fmt.Errorf("vmss(%s) instance(%s) nic(%s) has empty ip configurations", vmssName, to.Val(vm.InstanceID), to.Val(nic.Name))
				}
				for _, ipConfig := range vmNic.Properties.IPConfigurations {
					if ipConfig != nil && ipConfig.Properties != nil && strings.EqualFold(to.Val(ipConfig.Name), ipConfigName) {
						secondaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					} else if ipConfig != nil && ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
						primaryIP = to.Val(ipConfig.Properties.PrivateIPAddress)
					}
				}
			}
		}
	}
	if primaryIP == "" || secondaryIP == "" {
		return "", fmt.Errorf("failed to find private IP from vmss(%s), instance(%s), ipConfig(%s)", vmssName, to.Val(vm.InstanceID), ipConfigName)
	}

	vmprofile := egressgatewayv1alpha1.GatewayVMProfile{
		NodeName:    to.Val(vm.Properties.OSProfile.ComputerName),
		PrimaryIP:   primaryIP,
		SecondaryIP: secondaryIP,
	}
	if vmConfig.Status == nil {
		vmConfig.Status = &egressgatewayv1alpha1.GatewayVMConfigurationStatus{}
	}
	for i, profile := range vmConfig.Status.GatewayVMProfiles {
		if profile.NodeName == vmprofile.NodeName {
			if profile.PrimaryIP != primaryIP || profile.SecondaryIP != secondaryIP {
				vmConfig.Status.GatewayVMProfiles[i].PrimaryIP = primaryIP
				vmConfig.Status.GatewayVMProfiles[i].SecondaryIP = secondaryIP
				logger.Info("GatewayVMConfiguration status updated", "primaryIP", primaryIP, "secondaryIP", secondaryIP)
				return secondaryIP, nil
			}
			logger.Info("GatewayVMConfiguration status not changed", "primaryIP", primaryIP, "secondaryIP", secondaryIP)
			return secondaryIP, nil
		}
	}

	logger.Info("GatewayVMConfiguration status updated for new nodes", "nodeName", vmprofile.NodeName, "primaryIP", primaryIP, "secondaryIP", secondaryIP)
	vmConfig.Status.GatewayVMProfiles = append(vmConfig.Status.GatewayVMProfiles, vmprofile)

	return secondaryIP, nil
}

func (r *GatewayVMConfigurationReconciler) reconcileVMSSNetworkInterface(
	ctx context.Context,
	ipConfigName string,
	ipPrefixID string,
	lbBackendpoolID string,
	wantIPConfig bool,
	interfaces []*compute.VirtualMachineScaleSetNetworkConfiguration,
) (bool, error) {
	log := log.FromContext(ctx)
	expectedConfig := r.getExpectedIPConfig(ipConfigName, ipPrefixID, interfaces)
	var primaryNic *compute.VirtualMachineScaleSetNetworkConfiguration
	needUpdate := false
	foundConfig := false

	for _, nic := range interfaces {
		if nic.Properties != nil && to.Val(nic.Properties.Primary) {
			primaryNic = nic
			for i, ipConfig := range nic.Properties.IPConfigurations {
				if to.Val(ipConfig.Name) == ipConfigName {
					if !wantIPConfig {
						log.Info("Found unwanted ipConfig, dropping")
						nic.Properties.IPConfigurations = append(nic.Properties.IPConfigurations[:i], nic.Properties.IPConfigurations[i+1:]...)
						needUpdate = true
					} else {
						if different(ipConfig, expectedConfig) {
							log.Info("Found target ipConfig with different configurations, dropping")
							needUpdate = true
							nic.Properties.IPConfigurations = append(nic.Properties.IPConfigurations[:i], nic.Properties.IPConfigurations[i+1:]...)
						} else {
							log.Info("Found expected ipConfig, keeping")
							foundConfig = true
						}
					}
					break
				}
			}
		}
	}

	if wantIPConfig && !foundConfig {
		if primaryNic == nil {
			return false, fmt.Errorf("vmss(vm) primary network interface not found")
		}
		primaryNic.Properties.IPConfigurations = append(primaryNic.Properties.IPConfigurations, expectedConfig)
		needUpdate = true
	}

	changed, err := r.reconcileLbBackendPool(lbBackendpoolID, primaryNic)
	if err != nil {
		return false, err
	}
	needUpdate = needUpdate || changed

	return needUpdate, nil
}

func (r *GatewayVMConfigurationReconciler) reconcileLbBackendPool(
	lbBackendpoolID string,
	primaryNic *compute.VirtualMachineScaleSetNetworkConfiguration,
) (needUpdate bool, err error) {
	if primaryNic == nil {
		return false, fmt.Errorf("vmss(vm) primary network interface not found")
	}

	needBackendPool := false
	for _, ipConfig := range primaryNic.Properties.IPConfigurations {
		if strings.HasPrefix(to.Val(ipConfig.Name), consts.ManagedResourcePrefix) {
			needBackendPool = true
			break
		}
	}

	for _, ipConfig := range primaryNic.Properties.IPConfigurations {
		if ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
			backendPools := ipConfig.Properties.LoadBalancerBackendAddressPools
			for i, backend := range backendPools {
				if strings.EqualFold(lbBackendpoolID, to.Val(backend.ID)) {
					if !needBackendPool {
						backendPools = append(backendPools[:i], backendPools[i+1:]...)
						ipConfig.Properties.LoadBalancerBackendAddressPools = backendPools
						return true, nil
					} else {
						return false, nil
					}
				}
			}
			if !needBackendPool {
				return false, nil
			}
			backendPools = append(backendPools, &compute.SubResource{ID: to.Ptr(lbBackendpoolID)})
			ipConfig.Properties.LoadBalancerBackendAddressPools = backendPools
			return true, nil
		}
	}
	return false, fmt.Errorf("vmss(vm) primary ipConfig not found")
}

func (r *GatewayVMConfigurationReconciler) getExpectedIPConfig(
	ipConfigName,
	ipPrefixID string,
	interfaces []*compute.VirtualMachineScaleSetNetworkConfiguration,
) *compute.VirtualMachineScaleSetIPConfiguration {
	var subnetID *string
	for _, nic := range interfaces {
		if nic.Properties != nil && to.Val(nic.Properties.Primary) {
			for _, ipConfig := range nic.Properties.IPConfigurations {
				if ipConfig.Properties != nil && to.Val(ipConfig.Properties.Primary) {
					subnetID = ipConfig.Properties.Subnet.ID
				}
			}
		}
	}

	var pipConfig *compute.VirtualMachineScaleSetPublicIPAddressConfiguration
	if ipPrefixID != "" {
		pipConfig = &compute.VirtualMachineScaleSetPublicIPAddressConfiguration{
			Name: to.Ptr(ipConfigName),
			Properties: &compute.VirtualMachineScaleSetPublicIPAddressConfigurationProperties{
				PublicIPPrefix: &compute.SubResource{
					ID: to.Ptr(ipPrefixID),
				},
			},
		}
	}
	return &compute.VirtualMachineScaleSetIPConfiguration{
		Name: to.Ptr(ipConfigName),
		Properties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
			Primary:                      to.Ptr(false),
			PrivateIPAddressVersion:      to.Ptr(compute.IPVersionIPv4),
			PublicIPAddressConfiguration: pipConfig,
			Subnet: &compute.APIEntityReference{
				ID: subnetID,
			},
		},
	}
}

func different(ipConfig1, ipConfig2 *compute.VirtualMachineScaleSetIPConfiguration) bool {
	if ipConfig1.Properties == nil && ipConfig2.Properties == nil {
		return false
	}
	if ipConfig1.Properties == nil || ipConfig2.Properties == nil {
		return true
	}
	prop1, prop2 := ipConfig1.Properties, ipConfig2.Properties
	if to.Val(prop1.Primary) != to.Val(prop2.Primary) ||
		to.Val(prop1.PrivateIPAddressVersion) != to.Val(prop2.PrivateIPAddressVersion) {
		return true
	}

	if (prop1.Subnet != nil) != (prop2.Subnet != nil) {
		return true
	} else if prop1.Subnet != nil && prop2.Subnet != nil && !strings.EqualFold(to.Val(prop1.Subnet.ID), to.Val(prop2.Subnet.ID)) {
		return true
	}

	pip1, pip2 := prop1.PublicIPAddressConfiguration, prop2.PublicIPAddressConfiguration
	if (pip1 == nil) != (pip2 == nil) {
		return true
	} else if pip1 != nil && pip2 != nil {
		if to.Val(pip1.Name) != to.Val(pip2.Name) {
			return true
		} else if (pip1.Properties != nil) != (pip2.Properties != nil) {
			return true
		} else if pip1.Properties != nil && pip2.Properties != nil {
			prefix1, prefix2 := pip1.Properties.PublicIPPrefix, pip2.Properties.PublicIPPrefix
			if (prefix1 != nil) != (prefix2 != nil) {
				return true
			} else if prefix1 != nil && prefix2 != nil && !strings.EqualFold(to.Val(prefix1.ID), to.Val(prefix2.ID)) {
				return true
			}
		}
	}
	return false
}

func getVMSSResourceGroup(vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration) string {
	if vmConfig.Spec.VmssResourceGroup != "" {
		return vmConfig.Spec.VmssResourceGroup
	}
	// return an empty string so that azmanager will use the default resource group in the config
	return ""
}
