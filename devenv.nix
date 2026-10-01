{ pkgs, ... }:
{
  languages.go = {
    enable = true;
    package = pkgs.go_1_27;
  };

  env.CGO_ENABLED = "0";
}
