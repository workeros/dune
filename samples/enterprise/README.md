# Enterprise host composition

This package is a compileable composition boundary for a private Dune host. It accepts an application-owned identity provider, access checker, complete Managed provider, renewal policy and PostgreSQL credential refresher, then mounts the existing workbench beside the application's own routes.

The sample intentionally contains no permissive identity or authorization stub and no simulated cloud provider. Supply trusted adapters from private application code, keep credentials inside those adapters, and pass the returned `Handler` to the application's HTTP server. Start from `host.DefaultManagedWorkerOptions()` when overriding worker values, and advance both configuration versions when private semantics change.

This sample proves that the public Go packages can be composed from an unrelated module. It does not satisfy the real-provider lifecycle acceptance described in the enterprise plan.
