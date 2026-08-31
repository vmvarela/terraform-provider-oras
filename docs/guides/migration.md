---
page_title: "State Migration"
description: |-
  Migrate existing Terraform state to the ORAS backend.
---

# State Migration

The `oras` provider does not include a built-in migration tool. To migrate existing state from another backend (local, S3, GCS, Azure Blob, Consul, etc.) to the ORAS backend, use Terraform's built-in `state pull` and `state push` commands.

## Prerequisites

- Terraform 1.17+ alpha with `TF_ENABLE_PLUGGABLE_STATE_STORAGE=1`
- The `oras` provider configured and authenticated
- Access to the source backend (to pull current state)

## Migration Steps

### 1. Configure the ORAS Backend

Add the state store configuration to your Terraform configuration:

```hcl
terraform {
  required_providers {
    oras = {
      source  = "registry.terraform.io/vmvarela/oras"
      version = "~> 0.1"
    }
  }

  state_store "oras_oci" {
    provider = oras
    url      = "oci://ghcr.io/myorg/infra-tfstate"
    compression = true
    lock_ttl = "15m"
    max_versions = 10
  }
}

provider "oras" {}
```

### 2. Pull State from Current Backend

```bash
# Export current state to a file
export TF_ENABLE_PLUGGABLE_STATE_STORAGE=1
terraform state pull > current-state.tfstate
```

### 3. Initialize with New Backend

```bash
# Initialize with the ORAS backend configured
terraform init -migrate-state
```

When prompted "Do you want to copy existing state to the new backend?", answer **no** — we'll push manually for more control.

### 4. Push State to ORAS Backend

```bash
# Push the pulled state to the new ORAS backend
terraform state push current-state.tfstate
```

### 5. Verify Migration

```bash
# List workspaces to confirm state was stored
terraform workspace list

# Check state contents
terraform state list

# Run a plan to verify everything works
terraform plan
```

## Workspace Migration

If you use multiple workspaces, migrate each one:

```bash
# List current workspaces
terraform workspace list

# For each workspace:
for ws in default staging production; do
  terraform workspace select $ws
  terraform state pull > ${ws}-state.tfstate
  
  # Re-initialize with new backend (first time only)
  # terraform init -migrate-state
  
  terraform state push ${ws}-state.tfstate
done
```

## Migration from Specific Backends

### From Local Backend

```bash
# Local backend stores state in terraform.tfstate
terraform state pull > local-state.tfstate
terraform init -migrate-state  # Answer "no" to copy prompt
terraform state push local-state.tfstate
```

### From S3 Backend

```hcl
# Old configuration
terraform {
  backend "s3" {
    bucket = "my-tfstate-bucket"
    key    = "prod/terraform.tfstate"
    region = "us-east-1"
  }
}
```

```bash
terraform state pull > s3-state.tfstate
# Update configuration to use oras_oci state_store
terraform init -migrate-state  # Answer "no"
terraform state push s3-state.tfstate
```

### From Remote Backend (Terraform Cloud/Enterprise)

```bash
# If using Terraform Cloud remote backend
terraform state pull > tfc-state.tfstate
# Update configuration
terraform init -migrate-state  # Answer "no"
terraform state push tfc-state.tfstate
```

## Multi-Environment Migration

For organizations with multiple environments, use separate repositories:

```hcl
# Production
terraform {
  state_store "oras_oci" {
    provider = oras
    url      = "oci://ghcr.io/myorg/infra-tfstate-prod"
    max_versions = 20
  }
}

# Staging
terraform {
  state_store "oras_oci" {
    provider = oras
    url      = "oci://ghcr.io/myorg/infra-tfstate-staging"
    max_versions = 10
  }
}
```

Migrate each environment independently to maintain isolation.

## Verification Checklist

After migration, verify:

- [ ] `terraform workspace list` shows expected workspaces
- [ ] `terraform state list` shows all managed resources
- [ ] `terraform plan` shows no changes (no drift)
- [ ] `terraform apply` succeeds without modifications
- [ ] State appears in registry (check tags: `state-<workspace>`, `stver-<workspace>-v1`)
- [ ] Locking works: concurrent `terraform apply` shows lock conflict
- [ ] Version retention works (if `max_versions > 0`): older versions pruned after new applies

## Rollback Plan

If issues arise, you can rollback to the previous backend:

```bash
# 1. Pull state from ORAS
terraform state pull > oras-backup.tfstate

# 2. Restore old backend configuration
# (revert terraform block to previous backend)

# 3. Re-initialize with old backend
terraform init -migrate-state  # Answer "yes" to copy

# 4. Push state back
terraform state push oras-backup.tfstate
```

Keep the `oras-backup.tfstate` file until you're confident in the migration.

## Troubleshooting

### "State store does not support state locking"

Ensure you're using Terraform 1.17+ alpha with the experiment enabled.

### "Lock conflict" immediately after migration

The previous backend's lock may still exist. Wait for TTL expiry or manually clear:
- For S3: Delete the `.tflock` file in the bucket
- For Consul: Release the lock in Consul UI
- For local: Delete `.terraform.tfstate.lock.info`

### State size exceeds `max_state_size`

Increase the limit in your configuration:

```hcl
state_store "oras_oci" {
  max_state_size = 536870912  # 512 MiB
}
```

### GHCR: "delete:packages scope required"

Version pruning requires `delete:packages` scope. Either:
- Add the scope to your token, or
- Set `max_versions = 0` to disable pruning

## Best Practices

1. **Test in staging first** — Migrate a non-production environment first
2. **Backup before migrating** — Keep `terraform state pull` output as backup
3. **Migrate one workspace at a time** — Isolate risk
4. **Verify with `terraform plan`** — Ensure no drift before considering migration complete
5. **Monitor first few applies** — Watch for lock contention or retention issues
6. **Document the new backend** — Update team runbooks with ORAS-specific operations