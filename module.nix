{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.ilofan;
  format = pkgs.formats.json { };
  configFile = format.generate "ilofan.json" (
    cfg.settings
    // {
      socket = "/run/ilofan/ilofan.sock";
    }
  );
in
{
  options.services.ilofan = {
    enable = lib.mkEnableOption "the iLO 4 fan control daemon";

    package = lib.mkPackageOption pkgs "ilofan" { };

    passwordFile = lib.mkOption {
      type = lib.types.str;
      example = "/run/secrets/ilofan-password";
      description = ''
        File containing only the iLO password. It is passed with systemd
        LoadCredential and never copied to the Nix store. Use a string path,
        not a Nix path literal, or the secret ends up in the store.
      '';
    };

    settings = lib.mkOption {
      type = format.type;
      default = { };
      example = {
        host = "192.168.1.148";
        username = "ilofan";
        hostKey = "ssh-rsa AAAA...";
        tlsFingerprint = "2D:F7:...:FF";
        mode = "control";
        metricsAddress = ":9877";
      };
      description = "ilofan configuration. See the README for keys and defaults.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.settings ? host && cfg.settings ? username;
        message = "services.ilofan.settings needs host and username.";
      }
    ];

    environment.systemPackages = [ cfg.package ];

    users.groups.ilofan = { };
    users.users.ilofan = {
      isSystemUser = true;
      group = "ilofan";
    };

    systemd.services.ilofan = {
      description = "iLO 4 fan control";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} daemon -config ${configFile}";
        User = "ilofan";
        Group = "ilofan";
        LoadCredential = [ "password:${cfg.passwordFile}" ];
        RuntimeDirectory = "ilofan";
        RuntimeDirectoryMode = "0750";
        Restart = "on-failure";
        RestartSec = 10;
        # Leaves time to release the fans to firmware control on stop.
        TimeoutStopSec = 60;
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
      };
    };
  };
}
