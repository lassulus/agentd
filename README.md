# agentd

Runs coding agents behind a REST API. Every agent is a process that speaks
[ACP](https://agentclientprotocol.com) on stdio (Claude Code via
`claude-agent-acp`, `codex-acp`, `omp acp`, `pi-acp`, `hermes acp`, …);
agentd starts them, keeps each agent's history, and lets clients send
prompts, change model/effort/mode, and answer permission requests.

## API

Every request needs `Authorization: Bearer <token>`: an API token from the
tokens file, or an OAuth2 JWT access token from the configured issuer.

```
GET    /v1/harnesses                          configured agent commands
GET    /v1/harnesses/{name}/config            default config of new agents, {"thinking":"medium"}
PATCH  /v1/harnesses/{name}/config            {"thinking":"medium"} merges, {"thinking":null} removes
POST   /v1/agents                             {"harness","cwd","name","config":{…},"prompt"}
GET    /v1/agents[?all=1|?state=waiting]      open agents (all=1: also stopped ones)
GET    /v1/agents/{id}                        one agent, with its config options
DELETE /v1/agents/{id}[?purge=1]              stop it (purge: also delete the history)
POST   /v1/agents/{id}/messages               {"text"}; queued while a turn runs; resumes a stopped agent
POST   /v1/agents/{id}/cancel                 end the turn, drop queued messages
GET    /v1/agents/{id}/history?since=&wait=&limit=   events with seq > since; wait=N long-polls
GET    /v1/agents/{id}/events?since=          the same events as SSE (Last-Event-ID resumes)
GET    /v1/agents/{id}/transcript?last=20     markdown view
GET    /v1/agents/{id}/config                 model, effort, mode, … as the harness offers them
PATCH  /v1/agents/{id}/config                 {"model":"sonnet","effort":"high"}
GET    /v1/agents/{id}/permissions            pending permission requests and questions
POST   /v1/agents/{id}/permissions/{req}      {"option":"allow-once"} or, for questions, {"answer":{…}} / {"decline":true}
```

- A harness's default config (kept in the state dir) is applied to every new
  agent of it like a POST `config`, whose values win per key. Values are only
  checked then: a bad one fails the agent's creation. Running agents keep theirs.
- `cwd` must be inside the workspace root; a relative `cwd` is taken from it.
- States: `starting`, `idle`, `working`, `waiting` (a permission or question
  is pending), `stopped`, `error`.
- Permission requests stay open until someone answers or cancels; the first
  answer wins (409 afterwards). Nothing is auto-approved.
- History events: `user_message`, `acp` (the agent's `session/update`,
  unchanged), `permission_request`, `question`, `permission_resolved`,
  `config_changed`, `turn_end`, `state`.
- A stopped agent (also after an agentd restart) is resumed with ACP
  `session/resume`, or `session/load` if that is all the harness offers.

Example:

```sh
api() { curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
api -X POST $URL/v1/agents -d '{"harness":"claude","cwd":"myrepo","config":{"model":"sonnet"},"prompt":"run the tests"}'
api "$URL/v1/agents/a_xxxx/history?since=0&wait=30"
api -X POST $URL/v1/agents/a_xxxx/permissions/p_yyyy -d '{"option":"allow-once"}'
api -X PATCH $URL/v1/agents/a_xxxx/config -d '{"model":"opus"}'
```

## NixOS

```nix
{
  inputs.agentd.url = "github:lassulus/agentd";
  # …
  imports = [ inputs.agentd.nixosModules.default ];
  services.agentd = {
    enable = true;
    harnesses = {
      omp.command = [ "${pkgs.omp}/bin/omp" "acp" ];
      claude = {
        command = [ "${pkgs.claude-agent-acp}/bin/claude-agent-acp" ];
        env.CLAUDE_CODE_EXECUTABLE = "${pkgs.claude-code}/bin/claude";
      };
    };
    auth.tokensFile = "/run/secrets/agentd-tokens"; # lines: <user> <sha256 hex of token>
    auth.oidc = { issuer = "https://id.example.com"; audience = "<client id>"; };
    users = [ "<sub>" "alice@example.com" ];        # OAuth2 users (email only if verified)
  };
}
```

The service runs as its own `agentd` user with home `/var/lib/agentd`: the
harnesses' logins (`~/.claude`, `~/.omp`, …) live there, the history in
`/var/lib/agentd/agents/<id>/`, and agents work below `workspaceRoot`
(default `/var/lib/agentd/workspace`). Log a harness in once with e.g.
`runuser -u agentd -- omp`. agentd listens on `127.0.0.1:8790`; put a TLS
reverse proxy in front.

OAuth2 tokens must be JWTs (in Zitadel: the app's token type "JWT"); agentd
checks signature, issuer, audience and expiry.

## Web UI

`agentd-web` (same package, its own service) is a web UI for one or more
agentd instances: a list of the agents of all backends, a live transcript,
sending messages, cancelling a turn, answering permissions and questions,
and starting new agents.

Users log in with OIDC (authorization code flow with PKCE, as a public
client without secret). agentd-web keeps the access token in memory behind
a session cookie and sends it as bearer token to the backends, so each
agentd authorizes the user with its own `auth.oidc` and `users`. The
OIDC app's access tokens must be JWTs, and the backends' `audience` must
be in them (in Zitadel: the app's client id, or the project id). Zitadel's
access tokens carry no email, so list the users' `sub` in `users`.

```nix
services.agentd-web = {
  enable = true;
  url = "https://agents.example.com";          # redirect URI: <url>/callback
  backends = [ { name = "local"; url = "http://127.0.0.1:8790"; } ];
  oidc = { issuer = "https://id.example.com"; clientId = "<client id>"; };
};
services.agentd.auth.oidc = { issuer = "https://id.example.com"; audience = "<client id>"; };
services.agentd.users = [ "<sub>" ];
```

agentd-web listens on `127.0.0.1:8792`; put a TLS reverse proxy in front.
When the access token expires, the next request logs in again.

## Development

```sh
nix develop -c go test -race ./...   # or: nix flake check
```

The tests run the test binary itself as a fake ACP agent.
