# Deployment: from this Dockerfile to a cluster that is not a demo

`k8s/` deploys and is verified on kind by [`k8s/kind/verify.sh`](../k8s/kind/verify.sh) —
forty-seven assertions, and [`k8s/README.md`](../k8s/README.md) keeps the honest record of
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
the summary.

### Doing it without editing the files by hand

```sh
scripts/pin-images.sh out/ \
  ghcr.io/lai3d/ai-customer-service-go=sha256:428427f63d75… \
  ghcr.io/lai3d/ai-customer-service-go-admin-ui=sha256:9f1c0a5b2e88…
kubectl apply -f out/
```

It copies `k8s/*.yaml`, replaces each tag with the digest given for that repository, and
reads the result back to check nothing is still on a tag. The tag is **replaced** rather
than appended to: `repo:tag@sha256:…` is legal and is worse than either half, because the
tag becomes decoration a reader will trust and nothing checks.

It **refuses** rather than doing half the job — an image in `k8s/` with no digest given is
an error, not a line left as it was. A manifest set where some images are pinned and some
are not is the outcome that looks done.

`internal/deployment` runs the script on every `go test ./...`: the digests are arguments,
so it needs no cluster and no registry. A script nobody runs is a script that stops working,
and this one is only ever run by somebody in the middle of a release.

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

### It is base64, and here is the proof rather than the sentence

On the kind cluster, with a throwaway Secret whose value is a marker string:

```sh
kubectl -n kube-system exec etcd-ai-cs-go-control-plane -- etcdctl \
  --cacert /etc/kubernetes/pki/etcd/ca.crt \
  --cert /etc/kubernetes/pki/etcd/server.crt \
  --key /etc/kubernetes/pki/etcd/server.key \
  get /registry/secrets/ai-customer-service-go/ai-customer-service-go-secrets
```

Before the harness configured encryption, that returned the Secret's `data` map — and, via
the `last-applied-configuration` annotation, a second copy of it:

```
{"apiVersion":"v1","data":{"ANTHROPIC_API_KEY":"cGxhY2Vob2xkZXIt…","POSTGRES_PASSWORD":"Y3NhZ2VudA==","POSTGRES_USER":"Y3NhZ2VudA=="},…}
```

`Y3NhZ2VudA==` is `csagent`. An etcd backup is a copy of every key you hold, and *"a Secret
is base64"* is now a thing this repository has read rather than a thing it says.

**The first version of that probe was measuring nothing**, which is worth more than the
result. It piped `etcdctl` through `sh -c`, reported *0 occurrences of the marker*, and
looked like evidence of encryption. The etcd image is distroless: there is no `sh`, the exec
failed, and the empty output counted as an absence. The harness now asserts the encrypted
*prefix* as well as the absent plaintext, because a check that only looks for a missing
string also passes against an etcd it cannot read.

### And the harness turns it off

`k8s/kind/verify.sh` creates the cluster with an `EncryptionConfiguration` for `secrets`
(`aescbc`, key generated per run and gitignored) mounted into the API server, then asserts
both halves on a probe Secret: the value is **not** in etcd in the clear, and what is there
carries the `k8s:enc:aescbc:` prefix the API server writes in front of an encrypted value.

`aescbc` rather than KMS because a KMS provider needs a KMS. What that demonstrates is the
mechanism and the property; it does not solve key custody, which is the next section's
problem and is the harder half.

What a real deployment still needs one of:

- **Encryption at rest with a KMS provider**, so the key is not on the node beside the
  data it encrypts. The harness's key is: it proves the API server encrypts, and it would
  not survive somebody who has the disk.
- **External Secrets Operator** or **Secrets Store CSI**, so the value lives in AWS Secrets
  Manager / GCP Secret Manager / Vault and the cluster holds a reference.
- **Sealed Secrets**, if the values must be in git — encrypted to a controller key, so the
  repository holds ciphertext rather than base64.

All three are a change to how the Secret is *created*, not to any manifest here:
`deployment.yaml` takes the whole Secret through `envFrom` and does not care what wrote
it. That is the one piece of good news in this section.

## 6. Re-verify after any of it

```sh
k8s/kind/verify.sh            # a throwaway cluster, forty-seven assertions
k8s/kind/verify.sh --keep     # reuse the images already built
k8s/kind/verify.sh --down     # delete the cluster
```

If you change `resources`, re-run the memory sweep in `k8s/README.md` too — and remember
that on Go the CPU limit sets `GOMAXPROCS`, which sets the embedding concurrency bound, so
a change there moves a number that is not in the file you edited.

---

[← Back to the README](../README.md)
