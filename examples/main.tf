terraform {
  required_providers {
    oras = {
      source = "vmvarela/oras"
    }
  }

  # Pluggable state storage (experimental, alpha Terraform builds only).
  # Requires TF_ENABLE_PLUGGABLE_STATE_STORAGE=1 or `-enable-pluggable-state-storage-experiment`
  # on `terraform init`.
  state_store "oras_oci" {
    # Reference the provider declared below (do NOT use a nested provider block).
    provider = oras

    url = "oci://localhost:5001/estado"
  }
}

# Provider config. `insecure = true` makes the client talk plain HTTP
# (PlainHTTP) to the registry — required for local registries like Zot.
provider "oras" {
  insecure = true
}

resource "terraform_data" "example" {
  input = "hello"
}
