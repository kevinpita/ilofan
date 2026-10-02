{ pkgs, lib, ... }:
{
  # Also supplies gopls, Delve, staticcheck, and Go editor/generator tools.
  languages.go = {
    enable = true;
    package = pkgs.go_1_27;
  };

  packages = [
    pkgs.just
    pkgs.golangci-lint
    pkgs.stdenv.cc
    pkgs.pkg-config
    pkgs.gnumake
    pkgs.cmake
    pkgs.bashInteractive
    pkgs.coreutils
    pkgs.findutils
    pkgs.gnugrep
    pkgs.gnused
    pkgs.git
    pkgs.curl
    pkgs.cacert
    pkgs.jq
    pkgs.nixfmt
  ]
  ++ lib.optionals pkgs.stdenv.isLinux [ pkgs.gdb ];

  env = {
    CGO_ENABLED = "1";
    CC = "${pkgs.stdenv.cc}/bin/cc";
    CXX = "${pkgs.stdenv.cc}/bin/c++";
  };
}
