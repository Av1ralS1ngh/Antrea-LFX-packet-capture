.PHONY: build clean docker-build
 
BINARY_NAME=capture-controller
IMAGE_NAME=capture-controller
IMAGE_TAG=latest
 
build:
	CGO_ENABLED=0 GOOS=linux go build -o bin/$(BINARY_NAME) ./cmd/capture-controller
 
clean:
	rm -rf bin/
 
docker-build: build
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .
 
kind-load: docker-build
	kind load docker-image $(IMAGE_NAME):$(IMAGE_TAG) --name capture-test
 
deploy: kind-load
	kubectl apply -f deploy/daemonset.yaml
	kubectl apply -f deploy/test-pod.yaml
 
test:
	go test -v ./...
