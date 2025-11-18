from flask import Flask, jsonify, send_from_directory
from flask_cors import CORS
from kubernetes import client, config
import time
from datetime import datetime

app = Flask(__name__, static_folder='./frontend', static_url_path='/static')
CORS(app)

# Load Kubernetes config
try:
    config.load_incluster_config()
except:
    config.load_kube_config()

v1 = client.CoreV1Api()
apps_v1 = client.AppsV1Api()
networking_v1 = client.NetworkingV1Api()

@app.route('/')
def index():
    return send_from_directory('./frontend', 'index.html')

@app.route('/api/topology')
def get_topology():
    nodes_data = []
    edges_data = []
    
    # Get all namespaces
    namespaces = v1.list_namespace()
    for ns in namespaces.items:
        if ns.metadata.name not in ['kube-system', 'kube-public', 'kube-node-lease']:
            nodes_data.append({
                'id': f'ns-{ns.metadata.name}',
                'label': ns.metadata.name,
                'type': 'namespace',
                'namespace': ns.metadata.name,
                'status': 'healthy',
                'data': {
                    'name': ns.metadata.name,
                    'creationTimestamp': str(ns.metadata.creation_timestamp)
                }
            })
    
    # Get all nodes
    nodes = v1.list_node()
    for node in nodes.items:
        status = 'healthy'
        for condition in node.status.conditions:
            if condition.type == 'Ready' and condition.status != 'True':
                status = 'failed'
        
        nodes_data.append({
            'id': f'node-{node.metadata.name}',
            'label': node.metadata.name,
            'type': 'node',
            'status': status,
            'data': {
                'name': node.metadata.name,
                'capacity': {k: str(v) for k, v in node.status.capacity.items()},
                'conditions': [{'type': c.type, 'status': c.status} for c in node.status.conditions]
            }
        })
    
    # Get all pods
    pods = v1.list_pod_for_all_namespaces()
    for pod in pods.items:
        if pod.metadata.namespace in ['kube-system', 'kube-public']:
            continue
            
        status = 'healthy'
        if pod.status.phase != 'Running':
            status = 'degraded'
        
        for cs in (pod.status.container_statuses or []):
            if not cs.ready:
                status = 'failed'
        
        restarts = sum(cs.restart_count for cs in (pod.status.container_statuses or []))
        
        nodes_data.append({
            'id': f'pod-{pod.metadata.namespace}-{pod.metadata.name}',
            'label': pod.metadata.name,
            'type': 'pod',
            'namespace': pod.metadata.namespace,
            'status': status,
            'data': {
                'name': pod.metadata.name,
                'namespace': pod.metadata.namespace,
                'phase': pod.status.phase,
                'podIP': pod.status.pod_ip,
                'nodeName': pod.spec.node_name,
                'restarts': restarts,
                'labels': pod.metadata.labels or {}
            }
        })
        
        # Edge to node
        if pod.spec.node_name:
            edges_data.append({
                'id': f'edge-pod-node-{pod.metadata.namespace}-{pod.metadata.name}',
                'source': f'pod-{pod.metadata.namespace}-{pod.metadata.name}',
                'target': f'node-{pod.spec.node_name}',
                'type': 'pod-to-node',
                'data': {}
            })
        
        # Edge to namespace
        edges_data.append({
            'id': f'edge-ns-pod-{pod.metadata.namespace}-{pod.metadata.name}',
            'source': f'ns-{pod.metadata.namespace}',
            'target': f'pod-{pod.metadata.namespace}-{pod.metadata.name}',
            'type': 'namespace-to-pod',
            'data': {}
        })
    
    # Get all services
    services = v1.list_service_for_all_namespaces()
    for svc in services.items:
        if svc.metadata.namespace in ['kube-system', 'kube-public']:
            continue
            
        nodes_data.append({
            'id': f'svc-{svc.metadata.namespace}-{svc.metadata.name}',
            'label': svc.metadata.name,
            'type': 'service',
            'namespace': svc.metadata.namespace,
            'status': 'healthy',
            'data': {
                'name': svc.metadata.name,
                'namespace': svc.metadata.namespace,
                'type': svc.spec.type,
                'clusterIP': svc.spec.cluster_ip,
                'ports': [{'port': p.port, 'protocol': p.protocol} for p in (svc.spec.ports or [])]
            }
        })
        
        # Match services to pods
        if svc.spec.selector:
            for pod in pods.items:
                if pod.metadata.namespace == svc.metadata.namespace:
                    if all(pod.metadata.labels.get(k) == v for k, v in svc.spec.selector.items()):
                        edges_data.append({
                            'id': f'edge-svc-pod-{svc.metadata.namespace}-{svc.metadata.name}-{pod.metadata.name}',
                            'source': f'svc-{svc.metadata.namespace}-{svc.metadata.name}',
                            'target': f'pod-{pod.metadata.namespace}-{pod.metadata.name}',
                            'type': 'service-to-pod',
                            'data': {}
                        })
    
    return jsonify({
        'nodes': nodes_data,
        'edges': edges_data,
        'timestamp': datetime.now().isoformat()
    })

@app.route('/api/flows')
def get_flows():
    # Simulated flows for demo
    flows = []
    pods = v1.list_pod_for_all_namespaces()
    for i, pod in enumerate(pods.items[:10]):
        if pod.metadata.namespace in ['kube-system']:
            continue
        flows.append({
            'id': f'flow-{pod.metadata.namespace}-{pod.metadata.name}',
            'sourcePod': pod.metadata.name,
            'sourceNamespace': pod.metadata.namespace,
            'destPod': 'external',
            'destNamespace': '',
            'protocol': 'TCP',
            'bytesSent': 1000 + i * 100,
            'bytesReceived': 500 + i * 50,
            'packetsSent': 10 + i,
            'packetsReceived': 5 + i,
            'errors': 0,
            'timestamp': datetime.now().isoformat()
        })
    return jsonify(flows)

@app.route('/api/flows/metrics')
def get_flow_metrics():
    flows = get_flows().json
    total_bytes = sum(f['bytesSent'] + f['bytesReceived'] for f in flows)
    total_packets = sum(f['packetsSent'] + f['packetsReceived'] for f in flows)
    
    return jsonify({
        'totalFlows': len(flows),
        'totalBytes': total_bytes,
        'totalPackets': total_packets,
        'timestamp': datetime.now().isoformat()
    })

@app.route('/api/metrics/traffic')
def get_traffic_metrics():
    flows = get_flows().json
    
    # Calculate top talkers
    pod_traffic = {}
    for flow in flows:
        pod_key = f"{flow['sourceNamespace']}/{flow['sourcePod']}"
        if pod_key not in pod_traffic:
            pod_traffic[pod_key] = {'sent': 0, 'received': 0, 'total': 0}
        pod_traffic[pod_key]['sent'] += flow['bytesSent']
        pod_traffic[pod_key]['received'] += flow['bytesReceived']
        pod_traffic[pod_key]['total'] += flow['bytesSent'] + flow['bytesReceived']
    
    top_talkers = sorted(pod_traffic.items(), key=lambda x: x[1]['total'], reverse=True)[:10]
    
    # Protocol distribution
    protocol_dist = {}
    for flow in flows:
        proto = flow['protocol']
        protocol_dist[proto] = protocol_dist.get(proto, 0) + 1
    
    return jsonify({
        'totalBytesSent': sum(f['bytesSent'] for f in flows),
        'totalBytesReceived': sum(f['bytesReceived'] for f in flows),
        'topTalkers': [{'pod': k, **v} for k, v in top_talkers],
        'protocolDistribution': protocol_dist,
        'timestamp': datetime.now().isoformat()
    })

@app.route('/api/metrics/connections')
def get_connection_metrics():
    flows = get_flows().json
    
    # Connection pairs
    connection_pairs = {}
    for flow in flows:
        pair_key = f"{flow['sourcePod']} → {flow['destPod']}"
        if pair_key not in connection_pairs:
            connection_pairs[pair_key] = {'count': 0, 'bytes': 0}
        connection_pairs[pair_key]['count'] += 1
        connection_pairs[pair_key]['bytes'] += flow['bytesSent'] + flow['bytesReceived']
    
    top_pairs = sorted(connection_pairs.items(), key=lambda x: x[1]['count'], reverse=True)[:10]
    
    return jsonify({
        'activeConnections': len(flows),
        'connectionRate': len(flows) / 60.0,  # per minute, simulated
        'topConnectionPairs': [{'pair': k, **v} for k, v in top_pairs],
        'timestamp': datetime.now().isoformat()
    })

@app.route('/api/metrics/errors')
def get_error_metrics():
    flows = get_flows().json
    
    total_flows = len(flows)
    failed_flows = sum(1 for f in flows if f['errors'] > 0)
    error_rate = (failed_flows / total_flows * 100) if total_flows > 0 else 0
    
    # Pod error counts
    pod_errors = {}
    for flow in flows:
        if flow['errors'] > 0:
            pod_key = f"{flow['sourceNamespace']}/{flow['sourcePod']}"
            pod_errors[pod_key] = pod_errors.get(pod_key, 0) + flow['errors']
    
    problematic_pods = sorted(pod_errors.items(), key=lambda x: x[1], reverse=True)[:10]
    
    return jsonify({
        'errorRate': error_rate,
        'totalErrors': sum(f['errors'] for f in flows),
        'failedConnections': failed_flows,
        'problematicPods': [{'pod': k, 'errors': v} for k, v in problematic_pods],
        'timestamp': datetime.now().isoformat()
    })

@app.route('/api/flows/anomalies')
def get_anomalies():
    return jsonify([])

@app.route('/api/issues')
def get_issues():
    issues = []
    
    pods = v1.list_pod_for_all_namespaces()
    services = v1.list_service_for_all_namespaces()
    
    # 🔵 Scenario 1: Configuration Problems - Services with no matching pods
    misconfigured_services = []
    for svc in services.items:
        if svc.metadata.namespace in ['kube-system', 'kube-public']:
            continue
        if not svc.spec.selector:
            continue
        
        has_pods = False
        for pod in pods.items:
            if pod.metadata.namespace == svc.metadata.namespace:
                if all(pod.metadata.labels.get(k) == v for k, v in svc.spec.selector.items()):
                    has_pods = True
                    break
        
        if not has_pods:
            misconfigured_services.append(f"{svc.metadata.namespace}/{svc.metadata.name}")
    
    if misconfigured_services:
        issues.append({
            'id': 'issue-misconfigured-selectors',
            'type': 'configuration',
            'severity': 'high',
            'title': '🔵 Configuration Problems: Incorrect Service Selectors',
            'description': f'{len(misconfigured_services)} service(s) have selectors that don\'t match any pods',
            'remediation': 'Check service selectors and ensure they match pod labels correctly',
            'affectedResources': misconfigured_services[:5]
        })
    
    # 🔴 Scenario 2: Connectivity Failures - Pods in failed/pending state
    failed_pods = []
    for pod in pods.items:
        if pod.metadata.namespace in ['kube-system']:
            continue
        if pod.status.phase in ['Failed', 'Pending', 'Unknown']:
            failed_pods.append(f"{pod.metadata.namespace}/{pod.metadata.name}")
    
    if failed_pods:
        issues.append({
            'id': 'issue-connectivity-failures',
            'type': 'connectivity',
            'severity': 'critical',
            'title': '🔴 Connectivity Failures: Pods in Failed State',
            'description': f'{len(failed_pods)} pod(s) cannot start or reach services',
            'remediation': 'Check pod logs and events for connection errors',
            'affectedResources': failed_pods[:5]
        })
    
    # 🟠 Scenario 3: DNS Resolution Issues - Pods with custom DNS
    dns_issue_pods = []
    for pod in pods.items:
        if pod.metadata.namespace in ['kube-system']:
            continue
        if pod.spec.dns_policy == 'None':
            dns_issue_pods.append(f"{pod.metadata.namespace}/{pod.metadata.name}")
    
    if dns_issue_pods:
        issues.append({
            'id': 'issue-dns-resolution',
            'type': 'dns',
            'severity': 'high',
            'title': '🟠 DNS Resolution Issues: Custom DNS Configuration',
            'description': f'{len(dns_issue_pods)} pod(s) have custom DNS that may prevent cluster service discovery',
            'remediation': 'Review DNS configuration and ensure cluster DNS is accessible',
            'affectedResources': dns_issue_pods[:5]
        })
    
    # 🟡 Scenario 4: Network Policy Conflicts - Multiple policies on same pods
    try:
        policies = networking_v1.list_network_policy_for_all_namespaces()
        policy_map = {}
        
        for policy in policies.items:
            key = f"{policy.metadata.namespace}/{policy.spec.pod_selector.match_labels}"
            if key not in policy_map:
                policy_map[key] = []
            policy_map[key].append(policy.metadata.name)
        
        conflicts = [k for k, v in policy_map.items() if len(v) > 1]
        
        if conflicts:
            issues.append({
                'id': 'issue-policy-conflicts',
                'type': 'network-policy',
                'severity': 'medium',
                'title': '🟡 Network Policy Conflicts: Multiple Policies',
                'description': f'{len(conflicts)} pod selector(s) have multiple network policies which may conflict',
                'remediation': 'Review network policies and consolidate overlapping rules',
                'affectedResources': conflicts[:5]
            })
    except:
        pass
    
    # ⚪ Scenario 5: Security Gaps - No network policies
    try:
        policies = networking_v1.list_network_policy_for_all_namespaces()
        if len(policies.items) == 0:
            issues.append({
                'id': 'issue-no-network-policies',
                'type': 'security',
                'severity': 'medium',
                'title': '⚪ Security Gaps: No Network Policies',
                'description': 'Cluster has no network policies, all traffic is allowed by default',
                'remediation': 'Implement network policies to restrict pod-to-pod traffic',
                'affectedResources': ['Cluster-wide']
            })
    except:
        pass
    
    # 🟣 Scenario 6: Resource Constraints - Pods with restarts
    constrained_pods = []
    for pod in pods.items:
        if pod.metadata.namespace in ['kube-system']:
            continue
        restarts = sum(cs.restart_count for cs in (pod.status.container_statuses or []))
        if restarts > 3:
            constrained_pods.append(f"{pod.metadata.namespace}/{pod.metadata.name} ({restarts} restarts)")
    
    if constrained_pods:
        issues.append({
            'id': 'issue-resource-constraints',
            'type': 'resources',
            'severity': 'high',
            'title': '🟣 Resource Constraints: High Restart Count',
            'description': f'{len(constrained_pods)} pod(s) have excessive restarts indicating resource issues',
            'remediation': 'Check resource limits, OOMKills, and application health',
            'affectedResources': constrained_pods[:5]
        })
    
    # Check for pods without services (original check)
    unmatched = 0
    for pod in pods.items:
        if pod.metadata.namespace in ['kube-system']:
            continue
        matched = False
        for svc in services.items:
            if svc.metadata.namespace == pod.metadata.namespace and svc.spec.selector:
                if all(pod.metadata.labels.get(k) == v for k, v in svc.spec.selector.items()):
                    matched = True
                    break
        if not matched:
            unmatched += 1
    
    if unmatched > 0:
        issues.append({
            'id': 'issue-unmatched-pods',
            'type': 'configuration',
            'severity': 'low',
            'title': 'Pods without Services',
            'description': f'{unmatched} pods are not exposed by any service',
            'remediation': 'Create services for pods that need to be accessible',
            'affectedResources': ['Multiple pods']
        })
    
    return jsonify(issues)

if __name__ == '__main__':
    app.run(host='0.0.0.0', port=8080, debug=False)
