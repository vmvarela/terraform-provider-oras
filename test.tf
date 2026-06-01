terraform {
  required_providers {
    oras = {
      source = "vmvarela/oras"
    }
  }

  state_store "oras_oci" {
    url = "oci://mi-registry.com/estado"
  }
}
