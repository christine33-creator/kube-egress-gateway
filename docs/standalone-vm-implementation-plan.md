# Implementation Plan: Standalone VM Gateway Nodepool Support

## Overview

This document outlines the implementation plan for supporting standalone VMs as gateway nodepools in the kube-egress-gateway project. Currently, the system only supports Virtual Machine Scale Sets (VMSS) as gateway nodepools. With AKS planning to support standalone VM-based nodepools, we need to extend the system to support both VMSS and standalone VM configurations.

## Current Architecture Analysis

### Current VMSS-only Implementation

The current system assumes all gateway nodes are part of a VMSS:

1. **Node Identification**: Nodes are identified by VMSS-specific labels and provider IDs
2. **Resource Management**: Uses VMSS-specific Azure APIs for configuration
3. **IP Configuration**: Manages secondary IP configurations on VMSS instances
4. **Load Balancer Integration**: Associates VMSS with Azure Internal Load Balancer backend pools

### Key Components Affected

1. **API Types** (`api/v1alpha1/`)
   - `StaticGatewayConfiguration` - Currently has `GatewayVmssProfile`
   - `GatewayVMConfiguration` - Uses VMSS-specific terminology

2. **Controllers** (`controllers/manager/`)
   - `GatewayVMConfigurationReconciler` - VMSS-focused reconciliation logic
   - `StaticGatewayConfigurationReconciler` - Creates GatewayVMConfiguration

3. **Azure Manager** (`pkg/azmanager/`)
   - VMSS-specific client interfaces and operations
   - No individual VM management capabilities

4. **Constants and Labels** (`pkg/consts/`)
   - VMSS-specific constants and label definitions

## Implementation Plan

### Phase 1: Extend API Types

#### 1.1 Add Standalone VM Support to Configuration Types

**File**: `api/v1alpha1/staticgatewayconfiguration_types.go`

Add new profile type for standalone VMs:

```go
// GatewayStandaloneVMProfile defines standalone VM configuration
type GatewayStandaloneVMProfile struct {
    // Resource group containing the standalone VMs
    VMResourceGroup string `json:"vmResourceGroup"`
    
    // List of standalone VM names to use as gateways
    VMNames []string `json:"vmNames"`
    
    // Public IP prefix size to be applied to VMs
    //+kubebuilder:validation:Minimum=0
    //+kubebuilder:validation:Maximum=31
    PublicIpPrefixSize int32 `json:"publicIpPrefixSize"`
}

// GatewayProfile represents either VMSS or standalone VM configuration
type GatewayProfile struct {
    // VMSS-based gateway configuration (existing)
    // +optional
    VmssProfile *GatewayVmssProfile `json:"vmssProfile,omitempty"`
    
    // Standalone VM-based gateway configuration (new)
    // +optional
    StandaloneVMProfile *GatewayStandaloneVMProfile `json:"standaloneVMProfile,omitempty"`
}
```

Update `StaticGatewayConfigurationSpec`:

```go
type StaticGatewayConfigurationSpec struct {
    // Name of the gateway nodepool to apply the gateway configuration.
    // +optional
    GatewayNodepoolName string `json:"gatewayNodepoolName,omitempty"`

    // Gateway profile supporting both VMSS and standalone VMs
    // +optional
    GatewayProfile `json:"gatewayProfile,omitempty"`
    
    // DEPRECATED: Use GatewayProfile.VmssProfile instead
    // +optional
    GatewayVmssProfile `json:"gatewayVmssProfile,omitempty"`
    
    // ... rest of existing fields
}
```

#### 1.2 Add Validation Logic

Extend validation to ensure:
- Only one of VMSS or standalone VM profile is specified
- Standalone VM profile has valid VM names and resource group
- NodepoolName and VM profiles are mutually exclusive

### Phase 2: Extend Azure Resource Management

#### 2.1 Add VM Client to Azure Manager

**File**: `pkg/azmanager/azmanager.go`

```go
import (
    "sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient"
)

type AzureManager struct {
    *config.CloudConfig

    LoadBalancerClient   loadbalancerclient.Interface
    VmssClient           virtualmachinescalesetclient.Interface
    VmssVMClient         virtualmachinescalesetvmclient.Interface
    VMClient             virtualmachineclient.Interface // New
    PublicIPPrefixClient publicipprefixclient.Interface
    InterfaceClient      interfaceclient.Interface
    SubnetClient         subnetclient.Interface
}

func CreateAzureManager(cloud *config.CloudConfig, factory azclient.ClientFactory) (*AzureManager, error) {
    // ... existing code ...
    az.VMClient = factory.GetVirtualMachineClient()
    // ... rest of initialization
}
```

#### 2.2 Add VM-specific Operations

Add methods for managing standalone VMs:

```go
// ListVMs returns list of VMs in the specified resource group
func (az *AzureManager) ListVMs(ctx context.Context, resourceGroup string) ([]*compute.VirtualMachine, error)

// GetVM retrieves a specific VM
func (az *AzureManager) GetVM(ctx context.Context, resourceGroup, vmName string) (*compute.VirtualMachine, error)

// UpdateVM updates a VM configuration
func (az *AzureManager) UpdateVM(ctx context.Context, resourceGroup, vmName string, vm compute.VirtualMachine) (*compute.VirtualMachine, error)

// GetVMNetworkInterface retrieves network interface for a VM
func (az *AzureManager) GetVMNetworkInterface(ctx context.Context, resourceGroup, nicName string) (*network.Interface, error)
```

### Phase 3: Update Controllers

#### 3.1 Extend GatewayVMConfigurationReconciler

**File**: `controllers/manager/gatewayvmconfiguration_controller.go`

Add detection logic for VM type:

```go
func (r *GatewayVMConfigurationReconciler) isStandaloneVMMode(vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration) bool {
    // Check StaticGatewayConfiguration to determine mode
    gwConfig := &egressgatewayv1alpha1.StaticGatewayConfiguration{}
    if err := r.Get(ctx, req.NamespacedName, gwConfig); err != nil {
        return false
    }
    return gwConfig.Spec.GatewayProfile.StandaloneVMProfile != nil
}

func (r *GatewayVMConfigurationReconciler) reconcileStandaloneVMs(
    ctx context.Context,
    vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
    ipPrefixID string,
    wantIPConfig bool,
) ([]string, error) {
    // Implementation for standalone VM reconciliation
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
    // Implementation for individual VM reconciliation
}
```

#### 3.2 Update Node Matching Logic

Update node selection logic to handle both VMSS and standalone VMs:

```go
func (r *GatewayVMConfigurationReconciler) matchesGatewayProfile(
    node *corev1.Node,
    vmConfig *egressgatewayv1alpha1.GatewayVMConfiguration,
) bool {
    // Get the StaticGatewayConfiguration
    gwConfig := r.getStaticGatewayConfiguration(ctx, vmConfig)
    
    if gwConfig.Spec.GatewayProfile.VmssProfile != nil {
        return r.matchesVMSSProfile(node, gwConfig.Spec.GatewayProfile.VmssProfile)
    }
    
    if gwConfig.Spec.GatewayProfile.StandaloneVMProfile != nil {
        return r.matchesStandaloneVMProfile(node, gwConfig.Spec.GatewayProfile.StandaloneVMProfile)
    }
    
    return false
}

func (r *GatewayVMConfigurationReconciler) matchesStandaloneVMProfile(
    node *corev1.Node,
    profile *egressgatewayv1alpha1.GatewayStandaloneVMProfile,
) bool {
    // Extract VM name from node's provider ID or labels
    vmName := r.getVMNameFromNode(node)
    for _, configuredVM := range profile.VMNames {
        if vmName == configuredVM {
            return true
        }
    }
    return false
}
```

### Phase 4: Update Constants and Node Labels

#### 4.1 Add New Constants

**File**: `pkg/consts/const.go`

```go
const (
    // ... existing constants ...
    
    // Label key for standalone VM mode
    StandaloneVMNodeModeLabel = "kubernetes.azure.com/standalone-vm-mode"
    
    // Label value for standalone VM gateway nodes
    StandaloneVMNodeModeValue = "gateway"
)
```

#### 4.2 Update Node Selection Logic

Update the controller predicates to include standalone VM nodes:

```go
func resourceHasFilterLabel(m map[string]string) predicate.Funcs {
    return predicate.Funcs{
        CreateFunc: func(e event.CreateEvent) bool {
            return ifLabelMatch(e.Object, m) || ifStandaloneVMNode(e.Object)
        },
        DeleteFunc: func(e event.DeleteEvent) bool {
            return ifLabelMatch(e.Object, m) || ifStandaloneVMNode(e.Object)
        },
    }
}

func ifStandaloneVMNode(obj client.Object) bool {
    labels := obj.GetLabels()
    if v, ok := labels[consts.StandaloneVMNodeModeLabel]; ok {
        return strings.EqualFold(v, consts.StandaloneVMNodeModeValue)
    }
    return false
}
```

### Phase 5: Update Helm Charts and Documentation

#### 5.1 Update CRDs

**File**: `config/crd/bases/egressgateway.kubernetes.azure.com_staticgatewayconfigurations.yaml`

Regenerate CRDs to include new API fields.

#### 5.2 Update Sample Configurations

Create examples for standalone VM usage:

**File**: `config/samples/egressgateway_v1alpha1_staticgatewayconfiguration_standalone.yaml`

```yaml
apiVersion: egressgateway.kubernetes.azure.com/v1alpha1
kind: StaticGatewayConfiguration
metadata:
  name: standalone-vm-gateway
spec:
  gatewayProfile:
    standaloneVMProfile:
      vmResourceGroup: my-vm-rg
      vmNames:
        - gateway-vm-1
        - gateway-vm-2
      publicIpPrefixSize: 30
  provisionPublicIps: true
```

#### 5.3 Update Documentation

- Update design documentation to explain standalone VM support
- Add installation guide for standalone VM setup
- Update troubleshooting guide

### Phase 6: Testing and Validation

#### 6.1 Unit Tests

Add comprehensive unit tests for:
- New API validation logic
- Standalone VM reconciliation logic
- Node matching logic

#### 6.2 Integration Tests

Add end-to-end tests for:
- Standalone VM gateway configuration
- Mixed VMSS and standalone VM scenarios
- Migration scenarios

#### 6.3 Manual Testing

Create test environments with:
- Pure standalone VM setups
- Mixed VMSS and standalone VM setups
- Upgrade scenarios from VMSS-only to mixed mode

## Migration Strategy

### Backward Compatibility

1. **API Compatibility**: Keep existing `GatewayVmssProfile` field with deprecation notice
2. **Controller Logic**: Handle both old and new API formats
3. **Validation**: Allow gradual migration to new format

### Migration Path

1. **Phase 1**: Deploy new version with backward compatibility
2. **Phase 2**: Migrate existing configurations to new format
3. **Phase 3**: Remove deprecated fields in future version

## Risk Assessment

### Technical Risks

1. **Complexity Increase**: Supporting two VM types increases code complexity
2. **Testing Coverage**: Need comprehensive testing for all scenarios
3. **Azure API Differences**: Different APIs for VMSS vs standalone VMs

### Mitigation Strategies

1. **Abstraction Layer**: Create common interface for VM operations
2. **Comprehensive Testing**: Automated testing for all scenarios
3. **Phased Rollout**: Gradual deployment with monitoring

## Timeline

- **Week 1-2**: Implement API changes and validation
- **Week 3-4**: Extend Azure Manager with VM client support
- **Week 5-6**: Update controllers with standalone VM logic
- **Week 7-8**: Testing and documentation
- **Week 9**: Final integration and deployment

## Success Criteria

1. Support for standalone VM nodepools alongside existing VMSS support
2. Backward compatibility maintained
3. Comprehensive test coverage (>90%)
4. Updated documentation and examples
5. Successful migration path from VMSS-only to mixed mode
