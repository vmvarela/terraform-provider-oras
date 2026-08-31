# Plan: `terraform-provider-oras`

> Plugin experimental de Terraform que implementa `statestore.StateStore` para almacenar
> `tfstate` en OCI registries.

---

## Decisiones de diseño

| Decisión | Opción elegida |
|---|---|
| Módulo Go | `github.com/vmvarela/terraform-provider-oras` |
| Provider source | `registry.terraform.io/vmvarela/oras` |
| Config API | URL única: `url = "oci://registry/repo"` |
| Auth | A nivel del `provider` block (insecure, ca_file) |
| GHCR fallback | Incluido desde el principio |
| Terraform target | `1.17.0-alpha20260827` (`.terraform-version`) |

---

## Estado del proyecto

| Fase | Estado |
|---|---|
| **Fase 1** — Bootstrap (go.mod, main.go, dirs) | 🟢 Completada |
| **Fase 2** — Provider skeleton (ProviderWithStateStores) | 🟢 Completada |
| **Fase 3** — ORAS client | 🟢 Completada |
| **Fase 4** — StateStore implementation | 🟢 Completada |
| **Fase 5** — Testing (unit + Zot integration) | 🟢 Completada |
| **Fase 6** — Build tooling y setup local | 🟢 Completada |

---

## Estructura objetivo

```
terraform-provider-oras/
├── main.go                          # Entrada CLI del provider
├── go.mod                           # github.com/vmvarela/terraform-provider-oras
├── go.sum
├── Makefile
├── .terraform-version               # 1.17.0-alpha20260827
├── .envrc                           # TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
├── .terraformrc.dev                 # Dev overrides para pruebas locales
├── .goreleaser.yml                  # Release (GoReleaser) en tags v*
├── skills-lock.json                 # Hashes de skills
├── examples/
│   └── main.tf                      # Ejemplo funcional state_store "oras_oci"
├── .github/workflows/               # ci.yml (build+test+lint), release.yml, ...
├── PLAN.md                          # (este archivo)
├── internal/
│   ├── provider/
│   │   └── provider.go              # OrasProvider + ProviderWithStateStores
│   ├── statestore/
│   │   └── oci.go                   # statestore.StateStore implementation
│   ├── config/
│   │   └── config.go                # ProviderData compartido
│   └── oras/
│       ├── client.go                # ORAS client
│       ├── auth.go                  # Resolución de credenciales OCI
│       ├── ghcr.go                  # Fallback GHCR tag deletion
│       ├── helper_test.go           # fakeORASRepo in-memory
│       ├── auth_test.go
│       ├── client_test.go
│       ├── ghcr_test.go
│       └── zot_test.go              # Integración Zot (Docker)
```

---

## Fase 1 — Bootstrap

### Archivos a crear

- `go.mod` con dependencias:
  - `github.com/hashicorp/terraform-plugin-framework` v1.19.0
  - `oras.land/oras-go/v2`
  - `github.com/opencontainers/go-digest`
  - `github.com/opencontainers/image-spec`
- `main.go` — entry point con `providerserver.Serve`

### 🎯 Criterio de éxito

```bash
go build ./...
```

---

## Fase 2 — Provider skeleton

### Interfaces

```go
// Obligatorio: provider.Provider
func (p *OrasProvider) Metadata(...)  // TypeName = "oras"
func (p *OrasProvider) Schema(...)    // insecure (bool), ca_file (string)
func (p *OrasProvider) Configure(...) // crea http.Client y resuelve credenciales
func (p *OrasProvider) DataSources()  // nil
func (p *OrasProvider) Resources()    // nil

// Opcional: provider.ProviderWithStateStores
func (p *OrasProvider) StateStores() []func() statestore.StateStore
```

### 🎯 Criterio de éxito

`go build` compila sin errores y el binary `terraform-provider-oras` se genera.

---

## Fase 3 — ORAS client

### Componentes

| Archivo | Uso |
|---|---|
| `client.go` | Lógica core: push/pull, locking, versioning, compresión, retry |
| `backend.go` (parcial) | Resolución de credenciales, HTTP transport |
| `github.go` | GHCR tag deletion via GitHub REST API |

### API del client

```go
type Client struct { /* privado */ }

func NewClient(registry, repository string, opts ...Option) (*Client, error)

func (c *Client) Get(ctx context.Context, stateID string) ([]byte, error)
func (c *Client) Put(ctx context.Context, stateID string, data []byte) error
func (c *Client) Delete(ctx context.Context, stateID string) error
func (c *Client) Lock(ctx context.Context, stateID string, info LockInfo) (string, error)
func (c *Client) Unlock(ctx context.Context, stateID, lockID string) error
func (c *Client) List(ctx context.Context) ([]string, error)
```

### Tags OCI

| Tag | Propósito |
|---|---|
| `state-<workspace>` | Estado actual |
| `stver-<workspace>-v<N>` | Versiones históricas |
| `locked-<workspace>` | Lock activo |

### Media types

| Tipo | Descripción |
|---|---|
| `application/vnd.terraform.statefile.v1` | State sin comprimir |
| `application/vnd.terraform.statefile.v1+gzip` | State comprimido |
| `application/vnd.terraform.state.v1` | Artifact type del state |
| `application/vnd.terraform.lock.v1` | Artifact type del lock |

### Archivos a crear

- `internal/oras/client.go`
- `internal/oras/auth.go`
- `internal/oras/ghcr.go`

### 🎯 Criterio de éxito

```bash
go test -count=1 ./internal/oras/...
```

---

## Fase 4 — StateStore implementation

### Métodos obligatorios (statestore.StateStore)

| Método | Lógica |
|---|---|
| `Metadata` | `TypeName = req.ProviderTypeName + "_oci"` → `"oras_oci"` |
| `Schema` | `url` (required), `compression` (bool), `lock_ttl` (string), `max_versions` (int64), `max_state_size` (int64) |
| `Initialize` | Parsea URL → crea `oras.Client` → asigna a `resp.StateStoreData` |
| `Read` | `client.Get(ctx, req.StateID)` |
| `Write` | `client.Put(ctx, req.StateID, req.StateBytes)` |
| `Lock` | `client.Lock(ctx, req.StateID, lockInfo)` |
| `Unlock` | `client.Unlock(ctx, req.StateID, req.LockID)` |
| `GetStates` | `client.List(ctx)` |

### Métodos opcionales

- **`Configure`** — recibe `StateStoreData`, guarda `*oras.Client` en struct
- **`ValidateConfig`** — valida formato URL, rangos numéricos

### Configuración Terraform resultante

```hcl
terraform {
  required_providers {
    oras = {
      source = "vmvarela/oras"
    }
  }

  # state_store referencia el provider por nombre (NO usa bloque anidado)
  state_store "oras_oci" {
    provider = oras

    url          = "oci://ghcr.io/myorg/infra-state"
    compression  = true
    lock_ttl     = "15m"
    max_versions = 10
  }
}

# Provider config de nivel superior
provider "oras" {
  insecure = true
}
```

### Archivos a crear

- `internal/statestore/oci.go`

### 🎯 Criterio de éxito

```bash
go build ./...
# Y el examples/main.tf se puede validar con:
terraform init
```

---

## Fase 5 — Testing

### Unit tests (`internal/oras/*_test.go`)

| Test | Archivo |
|---|---|
| `fakeORASRepo` in-memory | `helper_test.go` |
| `Put/Get/Delete` round-trip | `client_test.go` |
| Locking concurrente y unlock mismatch | `client_test.go` |
| Compresión gzip | `client_test.go` |
| Stale lock TTL cleanup | `client_test.go` |
| Version tags y retention | `client_test.go` |
| Retry en errores transitorios | `client_test.go` |
| Parseo de credenciales | `backend_test.go` |

### Integration tests (Zot registry)

| Test | Archivo |
|---|---|
| State round-trip | `zot_test.go` |
| Lock/unlock concurrente | `zot_test.go` |
| Retention pruning | `zot_test.go` |
| Multi-workspace | `zot_test.go` |
| Gzip compression | `zot_test.go` |

### 🎯 Criterio de éxito

```bash
go test -race -count=1 ./...
TF_ORAS_ZOT_TEST=1 go test -race -v -timeout 120s ./internal/oras/... -run Zot
```

---

## Fase 6 — Build tooling y setup local

### Makefile targets

```makefile
build          # go build -o terraform-provider-oras .
install        # Instala en ~/.terraform.d/plugins/.../darwin_arm64/
test           # go test -race -count=1 ./...
test-zot       # Integration test contra Zot
lint           # golangci-lint run ./...
clean          # rm -f terraform-provider-oras
```

### `.terraformrc.dev`

```hcl
provider_installation {
  dev_overrides {
    "vmvarela/oras" = "/ruta/al/repo"
  }
  direct {}
}
```

### `examples/main.tf` (ejemplo funcional)

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

### 🎯 Criterio de éxito

```bash
make build && make test
# Terraform init funciona con el provider local
```

**Verificado e2e** (2026-08-27): Terraform `1.17.0-alpha20260827` + Zot local (HTTP).
Flujo `init → apply → destroy` funciona; el tag `state-default` se escribe/actualiza en el registry.
Notas del proveedor tras la prueba:
- `state_store` usa sintaxis de referencia `provider = oras` + `provider "oras" {}` de nivel superior (no block anidado).
- `insecure = true` en el provider fuerza `PlainHTTP` para registries `http://` locales (fix en `internal/statestore/oci.go`).
- `List` ignora 404 `name unknown` (repo sin state) para no romper `init` (fix en `internal/oras/client.go`).

---

## Dependencias entre fases

```
Fase 1 (go.mod + main.go)
    ↓
Fase 2 (provider skeleton)  ←→  Fase 3 (ORAS client)
    ↓                                  ↓
         Fase 4 (StateStore impl — une ambos)
              ↓
    Fase 5 (tests)  +  Fase 6 (tooling)
```

---

## Referencias

- [HashiCorp: State Store Implementation](https://developer.hashicorp.com/terraform/plugin/framework/state-stores/implementation)
- [oras-go v2](https://oras.land/) — librería OCI client
- [terraform-plugin-framework](https://github.com/hashicorp/terraform-plugin-framework) — v1.18+
- [Terraform alpha v1.17.0](https://github.com/hashicorp/terraform/tree/v1.17.0-alpha20260827) — estado experimental (PSS es alpha-only; releases estables no lo incluyen)
