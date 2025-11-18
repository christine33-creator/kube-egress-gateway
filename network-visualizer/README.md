# 🚀 Kubernetes Network Visualizer

Real-time interactive visualization and anomaly detection for Kubernetes clusters.

![Version](https://img.shields.io/badge/version-1.0.0-blue)
![License](https://img.shields.io/badge/license-MIT-green)

## 📖 Overview

Kubernetes Network Visualizer is a web-based tool that provides real-time visualization of your Kubernetes cluster topology, complete with intelligent anomaly detection. It helps DevOps teams, developers, and security engineers understand their cluster's network architecture and quickly identify configuration issues.

### ✨ Key Features

- **Interactive Graph Visualization** - Powered by Cytoscape.js
- **Real-time Updates** - Cluster state refreshed every 10 seconds
- **Smart Anomaly Detection** - Automatically detects 6 types of configuration issues
- **Advanced Search & Filtering** - Find resources quickly
- **Multiple Graph Layouts** - Force-directed, hierarchical, circular, and grid
- **Color-coded Health Status** - Instant visual feedback
- **Hover-to-Reveal Connections** - Clean UI that shows edges on demand

## 🎯 Problem Statement

Working with Kubernetes networking is challenging:
- No visual representation of cluster topology
- Configuration mistakes are hard to spot
- Debugging connectivity issues requires multiple kubectl commands
- Network policy conflicts are difficult to identify

This tool solves these problems with an intuitive, visual interface.

## 🏗️ Architecture

```
┌─────────────────┐
│   React UI      │  ← Cytoscape.js Graph Visualization
│  (Frontend)     │  ← Search, Filter, Layout Controls
└────────┬────────┘
         │ HTTP/WebSocket
┌────────▼────────┐
│  Flask API      │  ← Kubernetes Python Client
│   (Backend)     │  ← Anomaly Detection Engine
└────────┬────────┘
         │ REST API
┌────────▼────────┐
│  Kubernetes     │  ← Pods, Services, Nodes
│    Cluster      │  ← Network Policies, Events
└─────────────────┘
```

## 🚨 Anomaly Detection

The system automatically detects:

- 🔵 **Configuration Problems** - Services with selectors that don't match pods
- 🔴 **Connectivity Failures** - Pods in Failed/Pending state
- 🟠 **DNS Resolution Issues** - Custom DNS preventing service discovery
- 🟡 **Network Policy Conflicts** - Overlapping policies on same pods
- ⚪ **Security Gaps** - Missing network policies
- 🟣 **Resource Constraints** - Pods with excessive restarts

## 🚀 Quick Start

### Prerequisites

- Kubernetes cluster (Minikube, Kind, or any K8s cluster)
- kubectl configured
- Docker (for building images)

### Installation

1. **Clone the repository**
   ```bash
   git clone https://github.com/[your-username]/network-visualizer.git
   cd network-visualizer
   ```

2. **Build the Docker image**
   ```bash
   # For Minikube
   eval $(minikube docker-env)
   docker build -t network-visualizer:latest -f backend/Dockerfile.python .
   ```

3. **Deploy to Kubernetes**
   ```bash
   kubectl apply -f k8s/deployment.yaml
   ```

4. **Wait for pod to be ready**
   ```bash
   kubectl wait --for=condition=ready pod -l app=network-visualizer -n network-visualizer --timeout=60s
   ```

5. **Access the UI**
   ```bash
   kubectl port-forward -n network-visualizer svc/network-visualizer 8080:8080
   ```

6. **Open browser**
   ```
   http://localhost:8080
   ```

## 📚 Demo Application

To see the visualizer in action with sample microservices:

```bash
# Deploy demo application
kubectl apply -f demo-app.yaml

# Deploy anomaly scenarios
kubectl apply -f anomaly-scenarios.yaml
```

This creates:
- 3 Frontend pods
- 2 Backend API pods
- 1 Redis database
- 2 Payment service pods
- Traffic generators
- Several intentional misconfigurations for anomaly detection

## 🎨 Usage

### Interactive Graph

- **Hover** over nodes to reveal connections
- **Click** nodes to view detailed information
- **Drag** nodes to rearrange the graph
- **Zoom** and pan to explore

### Search & Filter

- **Search bar** - Type pod/service/namespace names
- **Status filter** - Show only healthy/degraded/failed resources
- **Type filter** - Filter by pods/services/nodes
- **Layout options** - Switch between different graph algorithms

### Detected Issues

Check the "DETECTED ISSUES" panel in the sidebar to see:
- Issue type and severity
- Affected resources
- Remediation recommendations

## 🛠️ Technology Stack

**Frontend:**
- React 18 with Hooks
- Cytoscape.js (graph visualization)
- WebSocket for real-time updates
- Vanilla CSS

**Backend:**
- Python 3.11
- Flask (web framework)
- Kubernetes Python Client
- Flask-CORS

**Infrastructure:**
- Docker
- Kubernetes RBAC
- Minikube (local development)

## 📊 API Endpoints

- `GET /api/topology` - Full cluster topology
- `GET /api/flows` - Network flow data
- `GET /api/flows/metrics` - Flow statistics
- `GET /api/metrics/traffic` - Traffic metrics
- `GET /api/metrics/connections` - Connection metrics
- `GET /api/metrics/errors` - Error metrics
- `GET /api/issues` - Detected anomalies
- `WS /ws/flows` - Real-time flow streaming

## 🔒 Security & RBAC

The application uses Kubernetes RBAC with:
- **ServiceAccount**: `network-visualizer`
- **ClusterRole**: Read-only access to:
  - Pods, Services, Nodes, Namespaces
  - Deployments, ReplicaSets, DaemonSets, StatefulSets
  - NetworkPolicies, Ingresses

No write permissions - completely read-only.

## 🔧 Development

### Backend Development

```bash
cd backend
python -m venv venv
source venv/bin/activate  # On Windows: venv\Scripts\activate
pip install -r requirements.txt
python app.py
```

### Frontend Development

The frontend is a single HTML file with inline JavaScript (React via CDN).
Edit `frontend/index.html` and refresh the browser.

### Building Locally

```bash
# Build with Docker
docker build -t network-visualizer:latest -f backend/Dockerfile.python .

# For Minikube
eval $(minikube docker-env)
docker build -t network-visualizer:latest -f backend/Dockerfile.python .
kubectl rollout restart deployment/network-visualizer -n network-visualizer
```

## 🔮 Future Enhancements

- [ ] Historical timeline and playback
- [ ] Real eBPF-based traffic capture (vs simulated)
- [ ] Multi-cluster support
- [ ] Prometheus integration
- [ ] AI-powered root cause analysis
- [ ] Export reports (PDF/JSON)
- [ ] Slack/Teams notifications
- [ ] What-if scenario simulator

## 🤝 Contributing

This is a hackathon project, but contributions are welcome!

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## 📝 License

MIT License - feel free to use this in your own projects!

## 🙏 Acknowledgments

- Built during [Hackathon Name]
- Inspired by Weave Scope, Cilium Hubble, and Kubernetes Dashboard
- Uses [Cytoscape.js](https://js.cytoscape.org/) for graph visualization
- Kubernetes Python Client by the Kubernetes team

## 📧 Contact

- GitHub: [@your-username]
- Email: your.email@example.com

## 📸 Screenshots

### Main Dashboard
![Main Dashboard](docs/screenshots/dashboard.png)

### Anomaly Detection
![Anomaly Detection](docs/screenshots/anomalies.png)

### Filtered View
![Filtered View](docs/screenshots/filtered.png)

---

**Built with ❤️ for the Kubernetes community**
