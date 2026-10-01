# Installing ilofan

ilofan is a single binary. You run `ilofan setup` once to create the config, then run `ilofan daemon` as a service.

Before you start, flash the patched iLO 4 firmware from [kendallgoto/ilo4_unlock](https://github.com/kendallgoto/ilo4_unlock). To check it, SSH into the iLO and run `fan info`.

## systemd

1. Install the binary:

   ```sh
   go install github.com/kevinpita/ilofan@latest
   sudo install -m 755 "$(go env GOPATH)/bin/ilofan" /usr/local/bin/
   ```

   With Nix, `nix build github:kevinpita/ilofan` builds the same binary into `./result/bin/ilofan`.

2. Create the config. This writes `/etc/ilofan/config.json` and `/etc/ilofan/password`, which only root can read:

   ```sh
   sudo ilofan setup
   ```

3. Create the service user and install the unit from [contrib/ilofan.service](../contrib/ilofan.service):

   ```sh
   sudo useradd --system --no-create-home ilofan
   sudo install -m 644 contrib/ilofan.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now ilofan
   ```

   The unit passes the password file to the daemon as a systemd credential, so it can stay readable only by root.

4. Let your user run the CLI without `sudo`:

   ```sh
   sudo usermod -aG ilofan "$USER"
   ```

Check it with `ilofan status` and `journalctl -u ilofan`.

## NixOS

Add the flake and enable the module. Generate the values once with `ilofan setup -config /tmp/ilofan.json`, or see [CONFIG.md](CONFIG.md#finding-the-ilo-keys).

```nix
{
  inputs.ilofan.url = "github:kevinpita/ilofan";

  outputs = { nixpkgs, ilofan, ... }: {
    nixosConfigurations.server = nixpkgs.lib.nixosSystem {
      modules = [
        ilofan.nixosModules.default
        {
          services.ilofan = {
            enable = true;
            passwordFile = "/run/secrets/ilofan-password";
            settings = {
              host = "192.168.1.148";
              username = "ilofan";
              hostKey = "ssh-rsa AAAA...";
              tlsFingerprint = "2D:F7:...:FF";
              mode = "control";
            };
          };
          users.users.me.extraGroups = [ "ilofan" ];
        }
      ];
    };
  };
}
```

`passwordFile` is a file containing only the iLO password. Create it however you manage secrets. Pass it as a string, not a Nix path like `./password`, or the password is copied into the Nix store. `settings` takes any key from [CONFIG.md](CONFIG.md).

## Other service managers

Run `ilofan daemon -config /etc/ilofan/config.json` as a user that can read `passwordFile`. The daemon creates its control socket at `/run/ilofan/ilofan.sock` by default, so that directory must exist and be writable by the daemon.
