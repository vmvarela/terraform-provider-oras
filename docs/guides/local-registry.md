---
page_title: "Local Registry (Zot)"
description: |-
  Run a local OCI registry with Zot for development and testing.
---

# Local Registry (Zot)

Zot is a lightweight, vendor-neutral OCI registry that's ideal for local development and testing with the `oras` provider. This guide covers running Zot locally and configuring the provider to use it.

## Quick Start

### 1. Start Zot

```bash
# Run Zot on port 5001 (plain HTTP)
docker run -d \
  --name zot \
  -p 5001:5001 \
  -v $(pwd)/zot-data:/var/lib/zot \
  ghcr.io/project-zot/zot-linux-amd64:v2.1.0
```

### 2. Configure Provider

```hcl
terraform {
  required_providers {
    oras = {
      source = "vmvarela/oras"
    }
  }

  state_store "oras_oci" {
    provider = oras
    url      = "oci://localhost:5001/estado"
  }
}

provider "oras" {
  insecure = true
}
```

### 3. Initialize and Apply

```bash
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
terraform init
terraform apply
```

## Zot Configuration Options

### With Authentication

Create a `config.yaml` for Zot with basic auth:

```yaml
# zot-config.yaml
http:
  address: "0.0.0.0"
  port: "5001"
  auth:
    htpasswd:
      path: /etc/zot/htpasswd
```

Generate htpasswd:
```bash
# Install apache2-utils for htpasswd
# Ubuntu/Debian: apt-get install apache2-utils
# macOS: brew install httpd
htpasswd -Bbn admin password123 > htpasswd
```

Run with auth:
```bash
docker run -d \
  --name zot \
  -p 5001:5001 \
  -v $(pwd)/zot-data:/var/lib/zot \
  -v $(pwd)/zot-config.yaml:/etc/zot/config.yaml:ro \
  -v $(pwd)/htpasswd:/etc/zot/htpasswd:ro \
  ghcr.io/project-zot/zot-linux-amd64:v2.1.0
```

Configure provider with credentials:
```bash
export ORAS_TOKEN=admin:password123  # or use ORAS_TOKEN env var
```

```hcl
provider "oras" {
  insecure = true
}
```

### With TLS (HTTPS)

Generate self-signed certs:
```bash
openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem -days 365 -nodes -subj "/CN=localhost"
```

Zot config with TLS:
```yaml
http:
  address: "0.0.0.0"
  port: "5001"
  tls:
    cert: /etc/zot/cert.pem
    key: /etc/zot/key.pem
```

Run with TLS:
```bash
docker run -d \
  --name zot \
  -p 5001:5001 \
  -v $(pwd)/zot-data:/var/lib/zot \
  -v $(pwd)/zot-config.yaml:/etc/zot/config.yaml:ro \
  -v $(pwd)/cert.pem:/etc/zot/cert.pem:ro \
  -v $(pwd)/key.pem:/etc/zot/key.pem:ro \
  ghcr.io/project-zot/zot-linux-amd64:v2.1.0
```

Configure provider with CA:
```hcl
provider "oras" {
  ca_file = "/path/to/cert.pem"
}
```

## Using the Example Configuration

The repository includes a ready-to-use example at [`examples/main.tf`](../examples/main.tf):

```hcl
terraform {
  required_providers {
    oras = {
      source = "vmvarela/oras"
    }
  }

  state_store "oras_oci" {
    provider = oras
    url      = "oci://localhost:5001/estado"
  }
}

provider "oras" {
  insecure = true
}

resource "terraform_data" "example" {
  input = "hello"
}
```

Run it:
```bash
# Terminal 1: Start Zot
docker run -d --name zot -p 5001:5001 -v $(pwd)/zot-data:/var/lib/zot ghcr.io/project-zot/zot-linux-amd64:v2.1.0

# Terminal 2: Run Terraform
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
cd examples
terraform init
terraform apply
```

## Integration Testing

The provider includes integration tests that spin up Zot automatically:

```bash
# Requires Docker
TF_ORAS_ZOT_TEST=1 make test-zot
```

This runs `go test -race -count=1 -tags=zot ./internal/oras/...` which starts a Zot container per test.

## Common Issues

### "connection refused" / "no such host"

- Ensure Zot is running: `docker ps | grep zot`
- Check port mapping: `-p 5001:5001`
- Verify URL matches: `oci://localhost:5001/...`

### "tls: failed to verify certificate" / "x509: certificate signed by unknown authority"

- Use `insecure = true` for plain HTTP
- Or provide `ca_file` with the self-signed cert

### "401 Unauthorized"

- For anonymous access: ensure Zot auth is disabled
- For basic auth: set `ORAS_TOKEN=username:password`
- Check Zot logs: `docker logs zot`

### "405 Method Not Allowed" on delete

- Zot supports manifest deletion by default
- This error typically only occurs with GHCR

## Cleanup

```bash
docker stop zot && docker rm zot
rm -rf zot-data  # Remove stored data
```

## Production Considerations

Zot is suitable for:
- Local development
- CI/CD ephemeral test registries
- Small team shared registries

For production workloads, consider:
- **Harbor** — Enterprise-grade with RBAC, replication, vulnerability scanning
- **GHCR / Docker Hub / ECR / ACR / GCR** — Managed cloud registries
- **Quay** — Red Hat's registry with geo-replication

The `oras` provider works identically across all OCI-compatible registries — only the URL and authentication change.