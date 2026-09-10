# Enterprise host composition

This package is a compileable composition boundary for a private Dune host. It accepts application-owned browser-session validation, access policy, a high-level Managed service and a PostgreSQL credential refresher, then mounts the existing workbench beside the application's own routes.

The sample intentionally contains no permissive identity or authorization stub and no simulated cloud provider. Supply trusted adapters from private application code, keep sessions and lifecycle state inside those adapters, implement `managed.Service.BindRunnerAccess` to receive Dune's narrow Runner/enrollment capability, and pass the returned `Handler` to the application's HTTP server.

This sample proves that the public Go packages can be composed from an unrelated module. It does not satisfy the real-provider lifecycle acceptance described in the enterprise plan.
