# idp-host on Kubernetes

Status: manifests only, **not yet applied to a cluster** (the reference host has none). `tests/deploy`
checks them for internal consistency; `kubectl apply --dry-run=server -k deploy/kubernetes` is the
next check when a cluster is available.

What is here:

| File | Content |
|---|---|
| `namespace.yaml` | `idp-host`, restricted pod security standard |
| `host.yaml` | ServiceAccount, env ConfigMap, providers/gateways ConfigMap (`host.yaml`, no secrets inside), Deployment (2 replicas, API + workers), Service |
| `migrate-job.yaml` | schema migrations as the schema owner, run before a rollout |
| `networkpolicy.yaml` | default deny; DNS; HOST ingress on 8450 (clients, ingress) and 9465 (observability); egress to PostgreSQL, the MCP gateway (`idp-mcp` namespace), the IdP, the model endpoints and the trace collector |
| `secrets.example.yaml` | the Secrets the manifests reference, with placeholders - create them with your secret management |
| `kustomization.yaml` | the overlay root; pin the image by digest |

Rollout order: `idp-host new-key` → Secrets → migrate Job → Deployment. Replicas claim runs from
the database with leases and fencing epochs, so scaling needs no coordination; a replica that dies
mid-run loses its lease after `IDP_HOST_LEASE` and another one continues the run.
