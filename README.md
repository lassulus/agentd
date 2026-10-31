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

## Development

```sh
nix develop -c go test -race ./...   # or: nix flake check
```

The tests run the test binary itself as a fake ACP agent.
