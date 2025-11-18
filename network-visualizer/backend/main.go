package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	flowsLock sync.RWMutex
	flows     []Flow
)

type TopologyResponse struct {
	Nodes     []TopologyNode `json:"nodes"`
	Edges     []TopologyEdge `json:"edges"`
	Timestamp string         `json:"timestamp"`
}

type TopologyNode struct {
	ID        string                 `json:"id"`
	Label     string                 `json:"label"`
	Type      string                 `json:"type"` // pod, service, node, namespace
	Namespace string                 `json:"namespace,omitempty"`
	Status    string                 `json:"status"` // healthy, degraded, failed
	Data      map[string]interface{} `json:"data"`
}

type TopologyEdge struct {
	ID          string                 `json:"id"`
	Source      string                 `json:"source"`
	Target      string                 `json:"target"`
	Type        string                 `json:"type"` // service-to-pod, pod-to-pod, traffic
	Data        map[string]interface{} `json:"data"`
	Animated    bool                   `json:"animated"`
	Bandwidth   float64                `json:"bandwidth,omitempty"`
	PacketsPerSec int                  `json:"packetsPerSec,omitempty"`
}

type Flow struct {
	ID            string    `json:"id"`
	SourcePod     string    `json:"sourcePod"`
	SourceNS      string    `json:"sourceNamespace"`
	DestPod       string    `json:"destPod"`
	DestNS        string    `json:"destNamespace"`
	Protocol      string    `json:"protocol"`
	BytesSent     int64     `json:"bytesSent"`
	BytesReceived int64     `json:"bytesReceived"`
	PacketsSent   int64     `json:"packetsSent"`
	PacketsRecv   int64     `json:"packetsReceived"`
	Errors        int       `json:"errors"`
	Timestamp     time.Time `json:"timestamp"`
}

type Anomaly struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"` // traffic_spike, port_scan, etc
	Severity    string    `json:"severity"`
	Description string    `json:"description"`
	AffectedPod string    `json:"affectedPod"`
	Metric      string    `json:"metric"`
	Baseline    float64   `json:"baseline"`
	Current     float64   `json:"current"`
	Timestamp   time.Time `json:"timestamp"`
}

type Issue struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // connectivity, policy_conflict, dns, etc
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Remediation string `json:"remediation"`
	AffectedResources []string `json:"affectedResources"`
}

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	r := mux.NewRouter()
	r.HandleFunc("/api/topology", corsMiddleware(getTopologyHandler(clientset))).Methods("GET", "OPTIONS")
	r.HandleFunc("/api/flows", corsMiddleware(getFlowsHandler)).Methods("GET", "OPTIONS")
	r.HandleFunc("/api/flows/metrics", corsMiddleware(getFlowMetricsHandler)).Methods("GET", "OPTIONS")
	r.HandleFunc("/api/flows/anomalies", corsMiddleware(getAnomaliesHandler)).Methods("GET", "OPTIONS")
	r.HandleFunc("/api/issues", corsMiddleware(getIssuesHandler(clientset))).Methods("GET", "OPTIONS")
	r.HandleFunc("/ws/flows", flowsWebSocketHandler)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("/app/frontend")))

	// Start background flow collector
	go collectFlows(clientset)

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func getTopologyHandler(clientset *kubernetes.Clientset) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		
		pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		services, err := clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		topology := buildTopology(pods, services, nodes, namespaces)
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(topology)
	}
}

func buildTopology(pods *corev1.PodList, services *corev1.ServiceList, nodes *corev1.NodeList, namespaces *corev1.NamespaceList) TopologyResponse {
	var topologyNodes []TopologyNode
	var edges []TopologyEdge

	// Add namespace nodes
	for _, ns := range namespaces.Items {
		topologyNodes = append(topologyNodes, TopologyNode{
			ID:     "ns-" + ns.Name,
			Label:  ns.Name,
			Type:   "namespace",
			Status: "healthy",
			Data: map[string]interface{}{
				"name": ns.Name,
				"creationTimestamp": ns.CreationTimestamp,
			},
		})
	}

	// Add Kubernetes nodes
	for _, node := range nodes.Items {
		status := "healthy"
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
				status = "failed"
			}
		}

		topologyNodes = append(topologyNodes, TopologyNode{
			ID:     "node-" + node.Name,
			Label:  node.Name,
			Type:   "node",
			Status: status,
			Data: map[string]interface{}{
				"name":          node.Name,
				"capacity":      node.Status.Capacity,
				"allocatable":   node.Status.Allocatable,
				"nodeInfo":      node.Status.NodeInfo,
				"conditions":    node.Status.Conditions,
			},
		})
	}

	// Add services
	for _, svc := range services.Items {
		if svc.Namespace == "kube-system" || svc.Namespace == "kube-public" {
			continue // Skip system services for cleaner view
		}

		topologyNodes = append(topologyNodes, TopologyNode{
			ID:        "svc-" + svc.Namespace + "-" + svc.Name,
			Label:     svc.Name,
			Type:      "service",
			Namespace: svc.Namespace,
			Status:    "healthy",
			Data: map[string]interface{}{
				"name":      svc.Name,
				"namespace": svc.Namespace,
				"type":      svc.Spec.Type,
				"clusterIP": svc.Spec.ClusterIP,
				"ports":     svc.Spec.Ports,
			},
		})
	}

	// Add pods and create edges
	for _, pod := range pods.Items {
		if pod.Namespace == "kube-system" || pod.Namespace == "kube-public" {
			continue
		}

		status := "healthy"
		if pod.Status.Phase != corev1.PodRunning {
			status = "degraded"
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if !cs.Ready {
				status = "failed"
			}
		}

		topologyNodes = append(topologyNodes, TopologyNode{
			ID:        "pod-" + pod.Namespace + "-" + pod.Name,
			Label:     pod.Name,
			Type:      "pod",
			Namespace: pod.Namespace,
			Status:    status,
			Data: map[string]interface{}{
				"name":       pod.Name,
				"namespace":  pod.Namespace,
				"phase":      pod.Status.Phase,
				"podIP":      pod.Status.PodIP,
				"nodeName":   pod.Spec.NodeName,
				"labels":     pod.Labels,
				"containers": pod.Spec.Containers,
				"restarts":   countRestarts(pod.Status.ContainerStatuses),
			},
		})

		// Edge from pod to node
		if pod.Spec.NodeName != "" {
			edges = append(edges, TopologyEdge{
				ID:     "edge-pod-node-" + pod.Namespace + "-" + pod.Name,
				Source: "pod-" + pod.Namespace + "-" + pod.Name,
				Target: "node-" + pod.Spec.NodeName,
				Type:   "pod-to-node",
				Data:   map[string]interface{}{},
			})
		}

		// Edge from namespace to pod
		edges = append(edges, TopologyEdge{
			ID:     "edge-ns-pod-" + pod.Namespace + "-" + pod.Name,
			Source: "ns-" + pod.Namespace,
			Target: "pod-" + pod.Namespace + "-" + pod.Name,
			Type:   "namespace-to-pod",
			Data:   map[string]interface{}{},
		})

		// Find matching services
		for _, svc := range services.Items {
			if svc.Namespace != pod.Namespace {
				continue
			}
			if matchesSelector(pod.Labels, svc.Spec.Selector) {
				edges = append(edges, TopologyEdge{
					ID:     "edge-svc-pod-" + svc.Namespace + "-" + svc.Name + "-" + pod.Name,
					Source: "svc-" + svc.Namespace + "-" + svc.Name,
					Target: "pod-" + pod.Namespace + "-" + pod.Name,
					Type:   "service-to-pod",
					Data:   map[string]interface{}{},
				})
			}
		}
	}

	return TopologyResponse{
		Nodes:     topologyNodes,
		Edges:     edges,
		Timestamp: time.Now().Format(time.RFC3339),
	}
}

func countRestarts(statuses []corev1.ContainerStatus) int {
	total := 0
	for _, cs := range statuses {
		total += int(cs.RestartCount)
	}
	return total
}

func matchesSelector(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func getFlowsHandler(w http.ResponseWriter, r *http.Request) {
	flowsLock.RLock()
	defer flowsLock.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(flows)
}

func getFlowMetricsHandler(w http.ResponseWriter, r *http.Request) {
	flowsLock.RLock()
	defer flowsLock.RUnlock()

	totalBytes := int64(0)
	totalPackets := int64(0)
	for _, flow := range flows {
		totalBytes += flow.BytesSent + flow.BytesReceived
		totalPackets += flow.PacketsSent + flow.PacketsRecv
	}

	metrics := map[string]interface{}{
		"totalFlows":   len(flows),
		"totalBytes":   totalBytes,
		"totalPackets": totalPackets,
		"timestamp":    time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

func getAnomaliesHandler(w http.ResponseWriter, r *http.Request) {
	// Simulated anomalies for demo
	anomalies := []Anomaly{}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anomalies)
}

func getIssuesHandler(clientset *kubernetes.Clientset) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		issues := []Issue{}

		// Check for pods without services
		pods, _ := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		services, _ := clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})

		unmatchedPods := 0
		for _, pod := range pods.Items {
			if pod.Namespace == "kube-system" {
				continue
			}
			matched := false
			for _, svc := range services.Items {
				if svc.Namespace == pod.Namespace && matchesSelector(pod.Labels, svc.Spec.Selector) {
					matched = true
					break
				}
			}
			if !matched {
				unmatchedPods++
			}
		}

		if unmatchedPods > 0 {
			issues = append(issues, Issue{
				ID:       "issue-unmatched-pods",
				Type:     "configuration",
				Severity: "low",
				Title:    "Pods without Services",
				Description: "Some pods are not exposed by any service",
				Remediation: "Create services for pods that need to be accessible",
				AffectedResources: []string{"Multiple pods"},
			})
		}

		// Check for network policies
		policies, _ := clientset.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{})
		if len(policies.Items) == 0 {
			issues = append(issues, Issue{
				ID:       "issue-no-network-policies",
				Type:     "security",
				Severity: "medium",
				Title:    "No Network Policies Defined",
				Description: "Cluster has no network policies, all traffic is allowed",
				Remediation: "Implement network policies to restrict pod-to-pod traffic",
				AffectedResources: []string{"Cluster-wide"},
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(issues)
	}
}

func collectFlows(clientset *kubernetes.Clientset) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Simulate flow collection (in production, would use eBPF/conntrack)
		ctx := context.Background()
		pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}

		newFlows := []Flow{}
		for i := 0; i < len(pods.Items) && i < 10; i++ {
			pod := pods.Items[i]
			if pod.Namespace == "kube-system" {
				continue
			}

			flow := Flow{
				ID:            "flow-" + pod.Namespace + "-" + pod.Name,
				SourcePod:     pod.Name,
				SourceNS:      pod.Namespace,
				DestPod:       "external",
				DestNS:        "",
				Protocol:      "TCP",
				BytesSent:     int64(1000 + i*100),
				BytesReceived: int64(500 + i*50),
				PacketsSent:   int64(10 + i),
				PacketsRecv:   int64(5 + i),
				Errors:        0,
				Timestamp:     time.Now(),
			}
			newFlows = append(newFlows, flow)
		}

		flowsLock.Lock()
		flows = newFlows
		flowsLock.Unlock()
	}
}

func flowsWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer conn.Close()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		flowsLock.RLock()
		data := flows
		flowsLock.RUnlock()

		if err := conn.WriteJSON(data); err != nil {
			log.Println("WebSocket write error:", err)
			return
		}
	}
}
