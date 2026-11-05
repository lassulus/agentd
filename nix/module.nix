{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.agentd;
  home = "/var/lib/agentd";
  configFile = pkgs.writeText "agentd.json" (
    builtins.toJSON {
      inherit (cfg) listen workspaceRoot users;
      stateDir = home;
      harnesses = lib.mapAttrs (_: h: { inherit (h) command env; }) cfg.harnesses;
      auth.oidc =
        let
          o = cfg.auth.oidc;
        in
        if o == null then
          null
        else
          {
            inherit (o) issuer audience;
            audienceFile = if o.audienceFile == null then null else "/run/credentials/agentd.service/audience";
          };
    }
  );
in
{
  options.services.agentd = {
    enable = lib.mkEnableOption "agentd, coding agents behind a REST API";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The agentd package (the flake's module sets it).";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8790";
      description = "Address the API listens on. Put a TLS reverse proxy in front.";
    };

    workspaceRoot = lib.mkOption {
      type = lib.types.str;
      default = "${home}/workspace";
      description = "Agents may only be started in directories below this one.";
    };

    harnesses = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            command = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              description = "Command that speaks ACP on stdio.";
              example = lib.literalExpression ''[ "''${pkgs.omp}/bin/omp" "acp" ]'';
            };
            env = lib.mkOption {
              type = lib.types.attrsOf lib.types.str;
              default = { };
              description = "Extra environment for this harness.";
            };
          };
        }
      );
      default = { };
      description = "Agents that can be started, by name.";
    };

    path = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = with pkgs; [
        bash
        coreutils
        findutils
        gnugrep
        gnused
        git
      ];
      description = "Packages on the agents' PATH.";
    };

    auth = {
      oidc = lib.mkOption {
        type = lib.types.nullOr (
          lib.types.submodule {
            options = {
              issuer = lib.mkOption {
                type = lib.types.str;
                description = "OIDC issuer whose JWT access tokens are accepted.";
              };
              audience = lib.mkOption {
                type = lib.types.nullOr lib.types.str;
                default = null;
                description = "Required token audience (the OAuth2 client id).";
              };
              audienceFile = lib.mkOption {
                type = lib.types.nullOr lib.types.str;
                default = null;
                description = ''
                  File with the audience instead, for a client id the IdP
                  generates. Read by systemd (LoadCredential), so it may be root-only.
                '';
              };
            };
          }
        );
        default = null;
        description = "Accept OAuth2 access tokens (JWT) from this issuer for {option}`services.agentd.users`.";
      };
      tokensFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = ''
          File with API tokens, one `<user> <sha256 hex of token>` per line.
          Read by systemd (LoadCredential), so it may be root-only.
        '';
      };
    };

    users = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "OAuth2 users allowed in: token `sub`, or email if the token marks it verified.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = lib.optional (cfg.auth.oidc != null) {
      assertion = (cfg.auth.oidc.audience == null) != (cfg.auth.oidc.audienceFile == null);
      message = "services.agentd.auth.oidc needs exactly one of audience and audienceFile";
    };

    users.users.agentd = {
      isSystemUser = true;
      group = "agentd";
      inherit home;
    };
    users.groups.agentd = { };

    systemd.tmpfiles.rules = [ "d ${cfg.workspaceRoot} 0700 agentd agentd -" ];

    systemd.services.agentd = {
      description = "agentd";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      inherit (cfg) path;
      environment.HOME = home;
      serviceConfig = {
        User = "agentd";
        Group = "agentd";
        StateDirectory = "agentd";
        StateDirectoryMode = "0700";
        WorkingDirectory = home;
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "-config"
            configFile
          ]
          ++ lib.optionals (cfg.auth.tokensFile != null) [
            "-tokens-file"
            "%d/tokens"
          ]
        );
        LoadCredential =
          lib.optional (cfg.auth.tokensFile != null) "tokens:${cfg.auth.tokensFile}"
          ++ lib.optional (
            cfg.auth.oidc != null && cfg.auth.oidc.audienceFile != null
          ) "audience:${cfg.auth.oidc.audienceFile}";
        # agentd stops its agents on SIGTERM; whatever is left gets killed after.
        KillMode = "mixed";
        Restart = "on-failure";
      };
    };
  };
}
