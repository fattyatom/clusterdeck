.PHONY: ui test build demo

ui:
	cd frontend && npm ci --no-audit --no-fund && npm run build

test: ui
	go vet ./...
	go test ./...
	cd frontend && npm test

build: ui
	CGO_ENABLED=0 go build -trimpath -o clusterdeck ./cmd/clusterdeck

demo: ui
	go run ./hack/demo
