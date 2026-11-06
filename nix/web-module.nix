{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.agentd-web;
  configFile = pkgs.writeText "agentd-web.json" (
    builtins.toJSON {
      inherit (cfg) listen url backends;
      oidc = {
        inherit (cfg.oidc) issuer clientId scopes;
        clientIdFile =
          if cfg.oidc.clientIdFile == null then null else "/run/credentials/agentd-web.service/client-id";
      };
    }
  );
in
{
  options.services.agentd-web = {
    enable = lib.mkEnableOption "agentd-web, a web UI for agentd backends";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The package with bin/agentd-web (the flake's module sets it).";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8792";
      description = "Address the UI listens on. Put a TLS reverse proxy in front.";
    };

    url = lib.mkOption {
      type = lib.types.str;
      example = "https://agents.example.com";
      description = "Public URL of the UI; the OIDC redirect URI is this plus `/callback`.";
    };

    backends = lib.mkOption {
      type = lib.types.listOf (
        lib.types.submodule {
          options = {
            name = lib.mkOption { type = lib.types.str; };
            url = lib.mkOption {
              type = lib.types.str;
              example = "http://127.0.0.1:8790";
            };
          };
        }
      );
      description = ''
        agentd instances to show. Each must accept the users' access tokens
        (its `auth.oidc` with the same issuer and an audience in the token).
      '';
    };

    oidc = {
      issuer = lib.mkOption {
        type = lib.types.str;
        description = "OIDC issuer to log in with.";
      };
      clientId = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Client id of a public client (no secret; PKCE) whose access tokens are JWTs.";
      };
      clientIdFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = ''
          File with the client id instead, for one the IdP generates. Read by
          systemd (LoadCredential), so it may be root-only.
        '';
      };
      scopes = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ "openid" ];
        description = "Scopes to request.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = (cfg.oidc.clientId == null) != (cfg.oidc.clientIdFile == null);
        message = "services.agentd-web.oidc needs exactly one of clientId and clientIdFile";
      }
    ];

    systemd.services.agentd-web = {
      description = "agentd-web";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/agentd-web -config ${configFile}";
        DynamicUser = true;
        LoadCredential = lib.optional (cfg.oidc.clientIdFile != null) "client-id:${cfg.oidc.clientIdFile}";
        Restart = "on-failure";
      };
    };
  };
}
