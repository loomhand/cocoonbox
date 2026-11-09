.PHONY: all api agent macos app image gate help

MACOS_TEST_ARGS ?=

all: gate api agent ## Linux gates: clean gate, api, agent (CI also runs image on Linux)

api: ## api: fmt check, lint, test, build
	$(MAKE) -C api fmt-check lint test build

agent: ## agent: fmt check, clippy -D warnings, test (CB_NO_BROWSER=1 skips the Chrome tests; the toolhost Node test runs only with NODE_BIN set)
	cd agent && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked

macos: ## macOS app: build the app and run the swift-testing suite
	cd macos && bash -c 'source scripts/toolchain.sh && swift build --product CocoonBox' && scripts/test.sh $(MACOS_TEST_ARGS)

app: ## macOS app bundle dist/CocoonBox.app for CB_ENDPOINT, ad hoc signed
	cd macos && scripts/bundle.sh

image: ## Syntax-check the shell scripts, ruff and pytest the Python, test cut.sh's dry run (Linux bash) and the package replay (docker)
	git ls-files -z image deploy scripts | xargs -0 awk 'FNR == 1 { if (/^#!.*[\/ ](ba)?sh( |$$)/) print FILENAME; nextfile }' | xargs -n1 bash -n
	{ git ls-files '*.py'; git ls-files -z image deploy scripts | xargs -0 awk 'FNR == 1 { if (/^#!.*python/) print FILENAME; nextfile }'; } | sort -u | xargs sh -c 'ruff format --check "$$@" && ruff check "$$@"' ruff
	python3 -m pytest -q deploy/host/tests
	bash image/test/cut-dry-run.sh
	bash image/test/packages.sh

gate: ## Clean gate over the tracked files and the dependencies; every proto field name is read and written somewhere in the Go, Rust or Swift sources (by name, not per message)
	scripts/clean-gate repo
	scripts/clean-gate deps
	scripts/proto-fields

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
