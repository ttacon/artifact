# HELP
# This will output the help for each task
# thanks to https://marmelab.com/blog/2016/02/29/auto-documented-makefile.html
.PHONY: help toml2cli cli

help: ## This help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)


toml2cli: ## Rebuild the CLI interface
	toml2cli -in-file=cmd/cli.toml -out-file=cmd/main.go

cli: toml2cli ## Make the CLI binary for local testing.
	CGO_ENABLED=0 go build -o artifact ./cmd
