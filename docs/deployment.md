# Deployment: from this Dockerfile to a cluster that is not a demo

`k8s/` deploys and is verified on kind by [`k8s/kind/verify.sh`](../k8s/kind/verify.sh) —
forty-five assertions, and [`k8s/README.md`](../k8s/README.md) keeps the honest record of
which of them have been *seen* to fail. This document is the part that happens off that
cluster: getting an image somewhere a real cluster can pull it, naming it in a way that
survives a year, and the three things the harness cannot verify at all.

## 1. The image

Two images, both built from this repository:

| | built from | what it is |
| --- | --- | --- |
| `ai-customer-service-go` | [`Dockerfile`](../Dockerfile) | the API. 1.1 GB, of which 470 MB is the embedding model baked in |
| `ai-customer-service-go-admin-ui` | `admin-ui/Dockerfile` | the operations UI: a static bundle on nginx |

Locally, which is what the harness does:

```sh
docker build -t ghcr.io/lai3d/ai-customer-service-go:0.1.0 .
docker build -t ghcr.io/lai3d/ai-customer-service-go-admin-ui:0.1.0 admin-ui
```

The API image is dominated by one file and that is a decision with a measurement behind
it: the model is in the image rather than downloaded at start-up, so a cold pod reaches
Ready in 4.4 s and needs no egress at all to serve. `docs/retrieval.md` has the trade
against calling an embedding API — 60 MB instead of 1.1 GB, plus a vendor, a key and a
network round trip on every query.

## 2. Publishing

[`.github/workflows/publish.yml`](../.github/workflows/publish.yml) runs on a `v*` tag and
pushes both images to GHCR with two tags — the version and `sha-<commit>` — plus an SBOM
and a signed build-provenance attestation. It prints the **digest** in the job summary,
because the digest is the only name in this whole path that cannot be made to mean
something else later.

```sh
git tag v0.2.0 && git push origin v0.2.0
```

Two things about that workflow are choices rather than defaults:

- **No `:latest`.** A moving tag makes "what is running in production" unanswerable: two
  pods of one Deployment started a week apart are two different builds and nothing says
  so. `k8s/kind/verify.sh` asserts that no manifest in `k8s/` carries a floating
  reference, and that assertion has been seen red.
- **`linux/amd64` only.** This image compiles cgo against a Rust static library and loads
  an ONNX Runtime shared object; an arm64 build under QEMU is that compile at roughly a
  tenth of native speed, on top of a 470 MB download. On arm64 you build locally — which
  is what the kind harness does on the machine this was developed on — or you pay for a
  native arm64 runner.

## 3. Pinning the manifests

`k8s/deployment.yaml` and `k8s/admin-ui.yaml` ship an explicit tag:

```yaml
image: ghcr.io/lai3d/ai-customer-service-go:0.1.0
```

For anything you care about, replace the tag with the digest the workflow printed:

```yaml
image: ghcr.io/lai3d/ai-customer-service-go@sha256:428427f63d75…
```

A tag records what somebody *meant* to deploy. A digest records what *is* deployed, and it
is the difference between a rollback that reproduces a state and a rollback that hopes the
registry still holds what it held. The rollout, if you would rather not edit the file:

```sh
kubectl -n ai-customer-service-go set image deploy/ai-customer-service-go \
  app=ghcr.io/lai3d/ai-customer-service-go@sha256:…
kubectl -n ai-customer-service-go rollout status deploy/ai-customer-service-go
```

**The kind harness cannot verify a digest**, and that is worth saying plainly rather than
leaving as a silence. `kind load docker-image` moves a locally built image by tag; a
manifest pinned to a registry digest would make every run pull from GHCR, which is not
what a throwaway cluster should do to check a manifest. So the harness verifies the tag is
explicit and not floating, and the digest is verified by whoever runs the workflow reading
the summary. That is a weaker guarantee, and it is where this stops.

## 4. What the manifests now include, and what each one still needs from you

| file | what it needs that a demo cluster does not have |
| --- | --- |
| `k8s/networkpolicy.yaml` | a CNI that enforces policy — Calico, Cilium, kindnet. Flannel does not, and nothing errors when it does not |
| `k8s/ingress.yaml` | an ingress controller, hostnames you own, and a certificate. All three are placeholders |
| `k8s/hpa.yaml` | metrics-server, or the HPA sits at `ScalingActive=False` for ever |
| `k8s/poddisruptionbudget.yaml` | nothing. It works on any cluster and does nothing at all until something drains a node |

The harness installs an ingress controller and metrics-server into its throwaway cluster
so the first three are exercised rather than asserted-by-reading. It applies `k8s/`
unmodified; the add-ons are cluster infrastructure, in the same category as the Postgres.

### The Ingress is the one to read before copying

It publishes the chat API on a hostname. `AUTH_MODE` in the ConfigMap is `off`, which
means client-supplied conversation ids and no ownership — so an Ingress in front of that
configuration is a way to read other people's conversations by guessing an id
([production-readiness item 1](production-readiness.md#1-anyone-can-read-anyone-elses-conversation)).
Turn identity on, or put something that authenticates in front of the host, before it
exists in DNS.

## 5. Secrets, which are still not solved

A Kubernetes Secret is base64. It is not encryption, it is an encoding, and `kubectl get
secret -o yaml` is one command away from the value for anyone with read access to the
namespace.

What this repository does: creates the Secret imperatively so the value is never in a file
git can see ([`k8s/README.md`](../k8s/README.md#apply)), and keeps the template out of the
directory apply path so `kubectl apply -f k8s/` cannot overwrite a working Secret with
placeholders — a mistake the Java implementation of this system made and measured.

What it does not do, and what a real deployment needs one of:

- **Encryption at rest for etcd** (`EncryptionConfiguration` with a KMS provider). Without
  it, an etcd backup is a copy of every API key you have.
- **External Secrets Operator** or **Secrets Store CSI**, so the value lives in AWS Secrets
  Manager / GCP Secret Manager / Vault and the cluster holds a reference.
- **Sealed Secrets**, if the values must be in git — encrypted to a controller key, so the
  repository holds ciphertext rather than base64.

All three are a change to how the Secret is *created*, not to any manifest here:
`deployment.yaml` takes the whole Secret through `envFrom` and does not care what wrote
it. That is the one piece of good news in this section.

## 6. Re-verify after any of it

```sh
k8s/kind/verify.sh            # a throwaway cluster, forty-five assertions
k8s/kind/verify.sh --keep     # reuse the images already built
k8s/kind/verify.sh --down     # delete the cluster
```

If you change `resources`, re-run the memory sweep in `k8s/README.md` too — and remember
that on Go the CPU limit sets `GOMAXPROCS`, which sets the embedding concurrency bound, so
a change there moves a number that is not in the file you edited.

---

[← Back to the README](../README.md)
