#!/bin/bash

set -e

echo "Building Docker image..."
cd backend
eval $(minikube docker-env)
docker build -t network-visualizer:latest -f Dockerfile ..
cd ..

echo "Applying Kubernetes manifests..."
kubectl apply -f k8s/deployment.yaml

echo "Waiting for pod to be ready..."
kubectl wait --for=condition=ready pod -l app=network-visualizer -n network-visualizer --timeout=120s

echo "Deployment complete!"
echo "Run: kubectl port-forward -n network-visualizer svc/network-visualizer 8080:8080"
echo "Then visit: http://localhost:8080"
