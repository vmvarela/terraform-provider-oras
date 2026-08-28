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
    provider "oras" {
      insecure = true
    }

    url = "oci://localhost:5001/estado"
  }
}

resource "terraform_data" "example" {
  input = "hello"
}
