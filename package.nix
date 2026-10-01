{ lib, buildGo127Module }:
buildGo127Module {
  pname = "ilofan";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = "sha256-1TZBU7538JaRZbNe4dWrtwVmLNWO+dyVIRSmsOvNtxM=";
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Fan curve daemon for HPE iLO 4 with the fan-control unlock patch";
    license = lib.licenses.mit;
    mainProgram = "ilofan";
    platforms = lib.platforms.linux;
  };
}
