terraform {
  required_providers {
    orastate = {
      source = "vmvarela/orastate"
    }
  }

  state_store "orastate_oci" {
    url = "oci://mi-registry.com/estado"
  }
}
