{
  description = "agentd: run ACP coding agents behind a REST API";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      agentdFor = pkgs: self.packages.${pkgs.stdenv.hostPlatform.system}.default;
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.callPackage ./nix/package.nix { };
      });

      nixosModules.default =
        { lib, pkgs, ... }:
        {
          imports = [
            ./nix/module.nix
            ./nix/web-module.nix
          ];
          services.agentd.package = lib.mkDefault (agentdFor pkgs);
          services.agentd-web.package = lib.mkDefault (agentdFor pkgs);
        };

      checks = forAll (pkgs: {
        package = agentdFor pkgs;
        # Evaluates the module and builds its unit file.
        module =
          let
            sys = nixpkgs.lib.nixosSystem {
              modules = [
                self.nixosModules.default
                {
                  nixpkgs.hostPlatform = pkgs.stdenv.hostPlatform.system;
                  boot.isContainer = true;
                  system.stateVersion = "25.11";
                  services.agentd = {
                    enable = true;
                    harnesses.demo.command = [ "${pkgs.coreutils}/bin/true" ];
                    auth.tokensFile = "/run/secrets/agentd-tokens";
                    auth.oidc = {
                      issuer = "https://id.example.com";
                      audience = "agentd";
                    };
                    users = [ "alice@example.com" ];
                  };
                  services.agentd-web = {
                    enable = true;
                    url = "https://agents.example.com";
                    backends = [
                      {
                        name = "local";
                        url = "http://127.0.0.1:8790";
                      }
                    ];
                    oidc = {
                      issuer = "https://id.example.com";
                      clientId = "agentd-web";
                    };
                  };
                }
              ];
            };
          in
          pkgs.linkFarm "agentd-module" {
            "agentd.service" = sys.config.systemd.units."agentd.service".unit;
            "agentd-web.service" = sys.config.systemd.units."agentd-web.service".unit;
          };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go ]; };
      });

      formatter = forAll (pkgs: pkgs.nixfmt);
    };
}
