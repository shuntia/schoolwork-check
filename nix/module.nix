# services.schoolwork-check — `schoolwork-check push` on a timer, as its own user.
#
# Non-secret settings go in `settings` (the variables of .env.example). Secrets
# are handed over with systemd LoadCredential so they never enter the Nix store
# and their source files can stay root-only; the Google refresh token is state
# (the program rewrites it) and lives in the state directory with the sync files.
#
# `schoolwork-check-ctl <args>` runs the CLI in the same sandbox, e.g.
# `sudo schoolwork-check-ctl google-login` or `sudo schoolwork-check-ctl push --dry-run`.
self:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.schoolwork-check;
  stateDir = "/var/lib/schoolwork-check";
  credEnv = {
    canvasToken = "CANVAS_TOKEN_FILE";
    noteToken = "NOTE_TOKEN_FILE";
    llmApiKey = "LLM_API_KEY_FILE";
    googleCredentials = "GOOGLE_CREDENTIALS_FILE";
  };
  given = lib.filterAttrs (_: v: v != null) cfg.credentials;
  environment = lib.mapAttrs (_: toString) cfg.settings // {
    XDG_STATE_HOME = "/var/lib";
    GOOGLE_TOKEN_FILE = "${stateDir}/google-token.json";
  };

  # Credentials land in the running unit's own directory, which differs between
  # the service and a schoolwork-check-ctl run.
  launch = pkgs.writeShellScript "schoolwork-check-launch" ''
    ${lib.concatStrings (lib.mapAttrsToList (k: _: ''
      export ${credEnv.${k}}="$CREDENTIALS_DIRECTORY/${k}"
    '') given)}
    exec ${lib.getExe cfg.package} "$@"
  '';

  serviceConfig = {
    User = cfg.user;
    Group = cfg.group;
    StateDirectory = "schoolwork-check";
    StateDirectoryMode = "0700";
    WorkingDirectory = stateDir;
    LoadCredential = lib.mapAttrsToList (k: path: "${k}:${path}") given;
    EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;
    UMask = "0077";
    NoNewPrivileges = true;
    PrivateTmp = true;
    PrivateDevices = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectControlGroups = true;
    RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
    RestrictNamespaces = true;
    LockPersonality = true;
    SystemCallArchitectures = "native";
    CapabilityBoundingSet = "";
  };

  ctl = pkgs.writeShellScriptBin "schoolwork-check-ctl" ''
    exec ${pkgs.systemd}/bin/systemd-run --quiet --pty --wait --collect \
      --unit=schoolwork-check-ctl \
      ${lib.concatStringsSep " \\\n      " (
        lib.mapAttrsToList (k: v: "--setenv=${lib.escapeShellArg "${k}=${v}"}") environment
        ++ lib.concatLists (lib.mapAttrsToList (k: v:
          map (x: "--property=${lib.escapeShellArg "${k}=${toString x}"}") (lib.toList v)) serviceConfig)
      )} \
      ${launch} "$@"
  '';
in
{
  options.services.schoolwork-check = {
    enable = lib.mkEnableOption "the schoolwork-check → note sync";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.schoolwork-check;
      defaultText = lib.literalExpression "schoolwork-check.packages.\${system}.schoolwork-check";
    };

    user = lib.mkOption { type = lib.types.str; default = "schoolwork-check"; };
    group = lib.mkOption { type = lib.types.str; default = "schoolwork-check"; };

    settings = lib.mkOption {
      type = with lib.types; attrsOf (oneOf [ str int bool ]);
      default = { };
      example = { CANVAS_BASE_URL = "https://school.instructure.com"; NOTE_BASE_URL = "http://127.0.0.1:3271"; };
      description = "Environment variables from .env.example that are not secret.";
    };

    credentials = lib.mapAttrs (_: description: lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      inherit description;
    }) {
      canvasToken = "File holding the Canvas access token.";
      noteToken = "File holding the note API token.";
      llmApiKey = "File holding the key for the OpenAI-compatible brief model.";
      googleCredentials = "The Google OAuth Desktop client JSON.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Extra environment read by systemd as root (e.g. private CALENDAR_DOCS / GOOGLE_CALENDARS).";
    };

    startAt = lib.mkOption {
      type = lib.types.str;
      default = "*-*-* 06..23/2:00:00";
    };
  };

  config = lib.mkIf cfg.enable {
    users.users = lib.mkIf (cfg.user == "schoolwork-check") {
      schoolwork-check = { isSystemUser = true; group = cfg.group; };
    };
    users.groups = lib.mkIf (cfg.group == "schoolwork-check") { schoolwork-check = { }; };

    environment.systemPackages = [ ctl ];

    systemd.services.schoolwork-check = {
      description = "Mirror Canvas and Google Classroom tasks into note";
      after = [ "network-online.target" "note.service" ];
      wants = [ "network-online.target" ];
      inherit environment;
      serviceConfig = serviceConfig // {
        Type = "oneshot";
        ExecStart = "${launch} push";
        # Attachment download + text extraction across every course is slow.
        TimeoutStartSec = "20min";
        Nice = 10;
        IOSchedulingClass = "idle";
      };
    };

    systemd.timers.schoolwork-check = {
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnCalendar = cfg.startAt;
        Persistent = true;
        RandomizedDelaySec = "5min";
      };
    };
  };
}
