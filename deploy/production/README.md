# Production deployment

The stack runs the application, PostgreSQL, and Cloudflare Tunnel. Only
`127.0.0.1:48080` is published on the host; public traffic enters through the
tunnel.

Copy `.env.example` to `.env` on the server and replace every value. Image
selection is intentionally not stored in `.env`; the workflow passes
`IMAGE_REGISTRY`, `IMAGE_NAMESPACE`, `IMAGE_NAME`, and `IMAGE_TAG` to Compose.

Operator guides:

- [Dynamic Cloudflare R2 account onboarding](R2-ACCOUNT-MANAGEMENT.md)
- [Index-mode third-party integration](INDEX-MODE-API.md)
- [OpenAPI 3.1 contract](index-mode-openapi.yaml)

Required GitHub secrets: `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_SSH_KEY`,
`DEPLOY_PORT`, `DEPLOY_PATH`, `DEPLOY_SERVICE`, `GHCR_USERNAME`, and
`GHCR_TOKEN`. Set repository variable `REGISTRY_TARGET=ghcr`.
