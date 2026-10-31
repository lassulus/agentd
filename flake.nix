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
          imports = [ ./nix/module.nix ];
          services.agentd.package = lib.mkDefault (agentdFor pkgs);
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
                }
              ];
            };
          in
          pkgs.linkFarm "agentd-module" {
            "agentd.service" = sys.config.systemd.units."agentd.service".unit;
          };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go ]; };
      });

      formatter = forAll (pkgs: pkgs.nixfmt);
    };
}
