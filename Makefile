.PHONY: all api agent deskhost macos gate help

all: gate api agent ## Gates that run on Linux: clean gate, api, agent

api: ## api: fmt check, lint, test, build
	$(MAKE) -C api fmt-check lint test build

agent: ## agent: fmt check, clippy -D warnings, test (CB_NO_BROWSER=1 skips the Chrome tests)
	cd agent && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked

deskhost: ## deskhost: fmt check, clippy -D warnings, test (macOS or Windows)
	cd deskhost && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked

macos: ## macOS app: build the app and run the swift-testing suite (needs ../swift-rfb)
	cd macos && bash -c 'source scripts/toolchain.sh && swift build --product CocoonBox' && scripts/test.sh

gate: ## Clean gate over the tracked files and the dependencies
	scripts/clean-gate repo
	scripts/clean-gate deps

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
