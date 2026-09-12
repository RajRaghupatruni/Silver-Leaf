.PHONY: fmt test build docker-build minikube-load install loadgen status uninstall generate

fmt:
	go fmt ./...

test:
	go test ./...

build:
	go build ./...

docker-build: build
	docker build -t optiscale/controller:dev -f Dockerfile.controller .
	docker build -t optiscale/demo-app:dev -f Dockerfile.demo-app .
	docker build -t optiscale/inventory-service:dev -f Dockerfile.inventory-service .

minikube-load:
	minikube image load optiscale/controller:dev
	minikube image load optiscale/demo-app:dev
	minikube image load optiscale/inventory-service:dev
	docker pull postgres:16.4-alpine
	minikube image load postgres:16.4-alpine

install:
	kubectl apply -f deploy/namespace.yaml
	kubectl apply -f config/crd/optiscale.silver-leaf.io_optiscalers.yaml
	kubectl wait --for=condition=Established --timeout=60s crd/optiscalers.optiscale.silver-leaf.io
	kubectl apply -f config/rbac/serviceaccount.yaml
	kubectl apply -f config/rbac/role.yaml
	kubectl apply -f config/prometheus/prometheus-rbac.yaml
	kubectl apply -f config/prometheus/prometheus-config.yaml
	kubectl apply -f config/prometheus/prometheus-deployment.yaml
	kubectl apply -k deploy/grafana
	kubectl apply -f deploy/postgres/configmap.yaml
	kubectl apply -f deploy/postgres/service.yaml
	kubectl apply -f deploy/postgres/deployment.yaml
	kubectl apply -f deploy/inventory/deployment.yaml
	kubectl apply -f deploy/inventory/service.yaml
	kubectl apply -f deploy/demo-app/deployment.yaml
	kubectl apply -f deploy/demo-app/service.yaml
	kubectl apply -f config/manager/optiscaler-deployment.yaml
	kubectl apply -f config/samples/optiscaler.yaml

loadgen:
	kubectl apply -f deploy/loadgen/deployment.yaml

status:
	kubectl get pods -n optiscale-system
	kubectl get pods -n optiscale-demo
	kubectl get deployments -n optiscale-demo
	kubectl get optiscalers -n optiscale-demo

uninstall:
	kubectl delete -f deploy/loadgen/deployment.yaml --ignore-not-found=true
	kubectl delete -k deploy/grafana --ignore-not-found=true
	kubectl delete -f config/samples/optiscaler.yaml --ignore-not-found=true
	kubectl delete -f config/manager/optiscaler-deployment.yaml --ignore-not-found=true
	kubectl delete -f deploy/inventory/service.yaml --ignore-not-found=true
	kubectl delete -f deploy/inventory/deployment.yaml --ignore-not-found=true
	kubectl delete -f deploy/postgres/deployment.yaml --ignore-not-found=true
	kubectl delete -f deploy/postgres/service.yaml --ignore-not-found=true
	kubectl delete -f deploy/postgres/configmap.yaml --ignore-not-found=true
	kubectl delete secret optiscale-postgres -n optiscale-demo --ignore-not-found=true
	kubectl delete -f deploy/demo-app/service.yaml --ignore-not-found=true
	kubectl delete -f deploy/demo-app/deployment.yaml --ignore-not-found=true
	kubectl delete -f config/prometheus/prometheus-deployment.yaml --ignore-not-found=true
	kubectl delete -f config/prometheus/prometheus-config.yaml --ignore-not-found=true
	kubectl delete -f config/prometheus/prometheus-rbac.yaml --ignore-not-found=true
	kubectl delete -f config/rbac/role.yaml --ignore-not-found=true
	kubectl delete -f config/rbac/serviceaccount.yaml --ignore-not-found=true
	kubectl delete -f config/crd/optiscale.silver-leaf.io_optiscalers.yaml --ignore-not-found=true
	kubectl delete -f deploy/namespace.yaml --ignore-not-found=true

generate:
	@echo "The checked-in CRD is maintained in config/crd."
