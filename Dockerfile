# syntax=docker/dockerfile:1
FROM node:22-alpine AS ui
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN mkdir -p ../web/dist && npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/clusterdeck ./cmd/clusterdeck

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/clusterdeck /clusterdeck
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/clusterdeck"]
