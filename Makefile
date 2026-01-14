.PHONY: help build run stop logs clean push

# Variables
IMAGE_NAME := mining-proxy
TAG := latest
REGISTRY := # Add your registry here (e.g., docker.io/username)

help: ## Show this help
	@echo "Available commands:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

build: ## Build Docker image
	docker build -t $(IMAGE_NAME):$(TAG) .

build-no-cache: ## Build Docker image without cache
	docker build --no-cache -t $(IMAGE_NAME):$(TAG) .

run: ## Run container with docker-compose
	docker-compose up -d

run-docker: ## Run container with docker (without compose)
	docker run -d \
		--name mining-proxy \
		--restart unless-stopped \
		-p 8080:8080 \
		$(IMAGE_NAME):$(TAG)

stop: ## Stop container
	docker-compose down

restart: ## Restart container
	docker-compose restart

logs: ## Show logs (follow)
	docker-compose logs -f

logs-tail: ## Show last 100 lines of logs
	docker-compose logs --tail=100

stats: ## Show container stats
	docker stats mining-proxy

shell: ## Open shell in running container (ash)
	docker exec -it mining-proxy /bin/sh

inspect: ## Inspect container
	docker inspect mining-proxy

clean: ## Remove container and image
	docker-compose down -v
	docker rmi $(IMAGE_NAME):$(TAG) || true

clean-all: ## Remove all mining-proxy images
	docker images | grep $(IMAGE_NAME) | awk '{print $$3}' | xargs docker rmi -f || true

push: build ## Build and push to registry
	docker tag $(IMAGE_NAME):$(TAG) $(REGISTRY)/$(IMAGE_NAME):$(TAG)
	docker push $(REGISTRY)/$(IMAGE_NAME):$(TAG)

pull: ## Pull image from registry
	docker pull $(REGISTRY)/$(IMAGE_NAME):$(TAG)

# Multi-architecture build (ARM + AMD64)
buildx: ## Build for multiple architectures
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		-t $(IMAGE_NAME):$(TAG) \
		--push \
		.

size: ## Show image size
	docker images $(IMAGE_NAME):$(TAG) --format "{{.Repository}}:{{.Tag}} - {{.Size}}"
