PYTHON_WASM_REPO     ?= goccy/python-wasm
PYTHON_WASM_VERSION  ?= v0.3.0
# python-wasm emits its release attestations from release.yml (the v* tag
# workflow), NOT build.yml — releasing lives only in release.yml there.
PYTHON_WASM_WORKFLOW ?= goccy/python-wasm/.github/workflows/release.yml

# Files pulled from the python-wasm release and verified against its SLSA
# attestation: the wasm2go bridge (the generated service binding, vendored as
# the internal package under the hand-written public API) and the embeddable
# stdlib zip. Asset name on the left, in-tree filename on the right (the
# rename is cosmetic; gh attestation verify matches by content digest).
BRIDGE_ASSET := python_wasm2go.go
BRIDGE_FILE  := internal/python.go
STDLIB_ASSET := python_stdlib.zip
STDLIB_FILE  := fs/stdlib.zip
RELEASE_URL       = https://github.com/$(PYTHON_WASM_REPO)/releases/download/$(PYTHON_WASM_VERSION)
ATTESTATION_API   = https://api.github.com/repos/$(PYTHON_WASM_REPO)/attestations

.PHONY: python download verify test

## python: refresh the bridge + stdlib from the upstream release and verify
## their GitHub artifact attestations. Run whenever PYTHON_WASM_VERSION bumps.
python: download verify

## download: fetch the wasm2go bridge and the stdlib zip from the upstream
## release and drop them in place at $(BRIDGE_FILE) / $(STDLIB_FILE).
download:
	curl -fSL --proto '=https' --tlsv1.2 -o $(BRIDGE_FILE) $(RELEASE_URL)/$(BRIDGE_ASSET)
	curl -fSL --proto '=https' --tlsv1.2 -o $(STDLIB_FILE) $(RELEASE_URL)/$(STDLIB_ASSET)

## verify: confirm each upstream-sourced file carries a valid GitHub artifact
## attestation signed by the upstream release.yml workflow. Each bundle is
## fetched anonymously from the public attestation API and verified offline via
## `gh attestation verify --bundle`. No GH access token is required.
verify:
	@set -eu; \
	root=$$(mktemp -d); \
	trap 'rm -rf $$root' EXIT; \
	for f in $(BRIDGE_FILE) $(STDLIB_FILE); do \
	  bundle=$$root/$$(echo $$f | tr / _).jsonl; \
	  digest=$$(shasum -a 256 $$f | awk '{print $$1}'); \
	  echo "==> fetching attestation bundle for $$f (sha256:$$digest)"; \
	  curl -fsSL --proto '=https' --tlsv1.2 \
	    "$(ATTESTATION_API)/sha256:$$digest" \
	    | jq -c '.attestations[].bundle' > $$bundle; \
	  echo "==> verifying $$f"; \
	  GH_TOKEN= GITHUB_TOKEN= gh attestation verify "$$f" \
	    -R $(PYTHON_WASM_REPO) \
	    --bundle $$bundle \
	    --signer-workflow $(PYTHON_WASM_WORKFLOW); \
	done

## test: run the Go test suite.
test:
	go test ./...
