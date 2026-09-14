# Security Principles

Treat credentials and Terraform state as sensitive.

- Never log tokens, credentials, state, or sensitive bodies.
- Prefer existing credential mechanisms over new provider secret fields.
- TLS verification is the default; insecure behavior must be explicit.
- Custom CA support must not silently disable normal verification.
- Keep required scopes minimal and document elevated permissions such as package deletion.
- Diagnostics must be useful without leaking secrets.
- Test authentication/TLS failure paths as well as success paths.
