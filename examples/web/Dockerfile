FROM node:24-bookworm AS ui-build

WORKDIR /src/examples/web/ui
RUN corepack enable && corepack prepare pnpm@11.22.0 --activate
COPY examples/web/ui/package.json examples/web/ui/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY examples/web/ui/ ./
RUN pnpm build

FROM golang:1.26 AS go-build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/web ./examples/web/server

FROM ubuntu:24.04 AS production

WORKDIR /app
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    apt-get clean && rm -rf /var/lib/apt/lists/*
COPY --from=go-build /out/web /app/web
COPY --from=ui-build /src/examples/web/ui/dist /app/ui/dist
EXPOSE 3000
ENTRYPOINT ["/app/web", "--port", "3000", "--vite-port", "0"]

FROM golang:1.26 AS server-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
EXPOSE 3000
ENTRYPOINT ["go", "run", "./examples/web/server", "--port", "3000", "--vite-port", "5173"]

FROM node:24-bookworm AS vite-dev

WORKDIR /app/examples/web/ui
RUN corepack enable && corepack prepare pnpm@11.22.0 --activate
COPY examples/web/ui/package.json examples/web/ui/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY examples/web/ui/ ./
EXPOSE 5173
ENTRYPOINT ["pnpm", "dev", "--host", "0.0.0.0"]
