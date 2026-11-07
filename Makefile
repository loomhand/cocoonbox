.PHONY: all api agent macos app image gate help

MACOS_TEST_ARGS ?=

all: gate api agent ## Gates that run on Linux: clean gate, api, agent

api: ## api: fmt check, lint, test, build
	$(MAKE) -C api fmt-check lint test build

agent: ## agent: fmt check, clippy -D warnings, test (CB_NO_BROWSER=1 skips the Chrome tests)
	cd agent && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked

macos: ## macOS app: build the app and run the swift-testing suite
	cd macos && bash -c 'source scripts/toolchain.sh && swift build --product CocoonBox' && scripts/test.sh $(MACOS_TEST_ARGS)

app: ## macOS app bundle dist/CocoonBox.app for CB_ENDPOINT, ad hoc signed
	cd macos && scripts/bundle.sh

image: ## Syntax-check the shell scripts, test cut.sh's dry run (Linux bash) and the package replay (docker)
	git ls-files -z image deploy scripts | xargs -0 awk 'FNR == 1 { if (/^#!.*[\/ ](ba)?sh( |$$)/) print FILENAME; nextfile }' | xargs -n1 bash -n
	python3 -c "import ast, sys; ast.parse(open(sys.argv[1]).read())" image/cut-proxy.py
	bash image/test/cut-dry-run.sh
	bash image/test/packages.sh

gate: ## Clean gate over the tracked files and the dependencies; every proto field has a reader and a writer
	scripts/clean-gate repo
	scripts/clean-gate deps
	scripts/proto-fields

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
