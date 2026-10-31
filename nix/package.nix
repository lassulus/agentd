{ lib, buildGoModule }:

buildGoModule {
  pname = "agentd";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ./..;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      (lib.fileset.fileFilter (f: f.hasExt "go") ./..)
    ];
  };

  vendorHash = "sha256-Ofh7ZXUaSAGrwBwR5agjjK6SljpK58fSwneHSTWviD8=";

  env.CGO_ENABLED = 0;
  ldflags = [ "-X main.version=0.1.0" ];

  meta = {
    description = "Run ACP coding agents behind a REST API";
    license = lib.licenses.mit;
    mainProgram = "agentd";
  };
}
