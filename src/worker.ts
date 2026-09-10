// Entry point for the Cloudflare Worker that fronts the backend
// Container. All it does is forward every request to the one running
// container instance — the actual routing/auth/BCP-proxy logic all
// lives in the existing Go server (cmd/server), unchanged.
import { Container, getContainer } from "@cloudflare/containers";

interface Env {
  BACKEND: DurableObjectNamespace<BackendContainer>;
  // Non-secret config, set in wrangler.jsonc's "vars".
  PORT: string;
  GOOGLE_REDIRECT_URL: string;
  FRONTEND_BASE_URL: string;
  COOKIE_SECURE: string;
  LOG_FILE: string;
  // Secrets, set via `wrangler secret put <NAME>` — never in
  // wrangler.jsonc.
  DATABASE_URL: string;
  GOOGLE_CLIENT_ID: string;
  GOOGLE_CLIENT_SECRET: string;
  ADMIN_EMAILS: string;
}

export class BackendContainer extends Container<Env> {
  defaultPort = 8080;
  // How long an idle container stays warm before Cloudflare stops it
  // (billing only applies while it's running) — 10 minutes balances
  // cold-start latency against cost for a low-traffic app.
  sleepAfter = "10m";

  // Forwarded into the container's environment exactly like
  // internal/config reads them locally (see .env.example).
  envVars: Record<string, string> = {
    PORT: this.env.PORT,
    DATABASE_URL: this.env.DATABASE_URL,
    GOOGLE_CLIENT_ID: this.env.GOOGLE_CLIENT_ID,
    GOOGLE_CLIENT_SECRET: this.env.GOOGLE_CLIENT_SECRET,
    GOOGLE_REDIRECT_URL: this.env.GOOGLE_REDIRECT_URL,
    FRONTEND_BASE_URL: this.env.FRONTEND_BASE_URL,
    COOKIE_SECURE: this.env.COOKIE_SECURE,
    LOG_FILE: this.env.LOG_FILE,
    ADMIN_EMAILS: this.env.ADMIN_EMAILS,
  };
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    // No name given — every request routes to the same fixed
    // ("cf-singleton-container") instance, which is all this app needs
    // at its current scale.
    const container = getContainer(env.BACKEND);
    return container.fetch(request);
  },
};
