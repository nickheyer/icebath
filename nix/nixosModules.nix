# NixOS module running the icebath gateway and backend as system services or in an OCI container
{ inputs, ... }:
{
  flake.nixosModules.default =
    {
      config,
      lib,
      options,
      pkgs,
      ...
    }:

    let
      cfg = config.services.icebath-agent;
      common = import ./moduleCommon.nix { inherit lib; };

      effectivePackage = common.effectivePackage cfg;
      icebath-agent = inputs.self.packages.${pkgs.stdenv.hostPlatform.system}.default;

      icebathHome = "${cfg.stateDir}/.icebath";

      effectiveWorkDir = if cfg.container.enable then containerWorkDir else cfg.workingDirectory;

      configYamlMode = if cfg.addToSystemPackages then "0660" else "0640";

      containerName = "icebath-agent";
      containerDataDir = "/data";
      containerHomeDir = "/home/icebath";

      containerBin =
        if cfg.container.backend == "docker" then
          "${pkgs.docker}/bin/docker"
        else
          "${pkgs.podman}/bin/podman";

      containerEntrypoint = pkgs.writeShellScript "icebath-container-entrypoint" ''
        set -eu

        ICEBATH_UID="''${ICEBATH_UID:?ICEBATH_UID must be set}"
        ICEBATH_GID="''${ICEBATH_GID:?ICEBATH_GID must be set}"

        EXISTING_GROUP=$(getent group "$ICEBATH_GID" 2>/dev/null | cut -d: -f1 || true)
        if [ -n "$EXISTING_GROUP" ]; then
          GROUP_NAME="$EXISTING_GROUP"
        else
          GROUP_NAME="icebath"
          if command -v groupadd >/dev/null 2>&1; then
            groupadd -g "$ICEBATH_GID" "$GROUP_NAME"
          elif command -v addgroup >/dev/null 2>&1; then
            addgroup -g "$ICEBATH_GID" "$GROUP_NAME" 2>/dev/null || true
          fi
        fi

        PASSWD_ENTRY=$(getent passwd "$ICEBATH_UID" 2>/dev/null || true)
        if [ -n "$PASSWD_ENTRY" ]; then
          TARGET_USER=$(echo "$PASSWD_ENTRY" | cut -d: -f1)
          TARGET_HOME=$(echo "$PASSWD_ENTRY" | cut -d: -f6)
        else
          TARGET_USER="icebath"
          TARGET_HOME="/home/icebath"
          if command -v useradd >/dev/null 2>&1; then
            useradd -u "$ICEBATH_UID" -g "$ICEBATH_GID" -m -d "$TARGET_HOME" -s /bin/bash "$TARGET_USER"
          elif command -v adduser >/dev/null 2>&1; then
            adduser -u "$ICEBATH_UID" -D -h "$TARGET_HOME" -s /bin/sh -G "$GROUP_NAME" "$TARGET_USER" 2>/dev/null || true
          fi
        fi
        mkdir -p "$TARGET_HOME"
        chown "$ICEBATH_UID:$ICEBATH_GID" "$TARGET_HOME"
        chmod 0750 "$TARGET_HOME"

        if [ -n "''${ICEBATH_HOME:-}" ] && [ -d "$ICEBATH_HOME" ]; then
          find "$ICEBATH_HOME" \! -user "$ICEBATH_UID" -exec chown "$ICEBATH_UID:$ICEBATH_GID" {} +
        fi

        if [ ! -f /var/lib/icebath-tools-provisioned ] && command -v apt-get >/dev/null 2>&1; then
          echo "First boot: provisioning agent tools..."
          apt-get update -qq
          apt-get install -y -qq sudo curl ca-certificates gnupg
          mkdir -p /etc/apt/keyrings
          curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
            | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
          echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
            > /etc/apt/sources.list.d/nodesource.list
          apt-get update -qq
          apt-get install -y -qq nodejs
          touch /var/lib/icebath-tools-provisioned
        fi

        if command -v sudo >/dev/null 2>&1 && [ ! -f /etc/sudoers.d/icebath ]; then
          mkdir -p /etc/sudoers.d
          echo "$TARGET_USER ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/icebath
          chmod 0440 /etc/sudoers.d/icebath
        fi

        if ! command -v uv >/dev/null 2>&1 && [ ! -x "$TARGET_HOME/.local/bin/uv" ] && command -v curl >/dev/null 2>&1; then
          su -s /bin/sh "$TARGET_USER" -c 'curl -LsSf https://astral.sh/uv/install.sh | sh' || true
        fi

        _UV_BIN="$TARGET_HOME/.local/bin/uv"
        if [ ! -d "$TARGET_HOME/.venv" ] && [ -x "$_UV_BIN" ]; then
          su -s /bin/sh "$TARGET_USER" -c "
            export PATH=\"\$HOME/.local/bin:\$PATH\"
            uv python install 3.12
            uv venv --python 3.12 --seed \"\$HOME/.venv\"
          " || true
        fi

        if [ -d "$TARGET_HOME/.venv/bin" ]; then
          export PATH="$TARGET_HOME/.venv/bin:$PATH"
        fi

        if command -v setpriv >/dev/null 2>&1; then
          exec setpriv --reuid="$ICEBATH_UID" --regid="$ICEBATH_GID" --init-groups "$@"
        elif command -v su >/dev/null 2>&1; then
          exec su -s /bin/sh "$TARGET_USER" -c 'exec "$0" "$@"' -- "$@"
        else
          echo "WARNING: no privilege-drop tool (setpriv/su), running as root" >&2
          exec "$@"
        fi
      '';

      containerIdentity = builtins.hashString "sha256" (
        builtins.toJSON {
          schema = 4;
          image = cfg.container.image;
          extraVolumes = cfg.container.extraVolumes;
          extraOptions = cfg.container.extraOptions;
        }
      );

      identityFile = "${cfg.stateDir}/.container-identity";

      containerModeFile = pkgs.writeText "icebath-container-mode" ''
        backend=${cfg.container.backend}
        container_name=${containerName}
        exec_user=${cfg.user}
        icebath_bin=${containerDataDir}/current-package/bin/icebath
      '';

      containerWorkDir =
        if lib.hasPrefix "${cfg.stateDir}/" cfg.workingDirectory then
          "${containerDataDir}/${lib.removePrefix "${cfg.stateDir}/" cfg.workingDirectory}"
        else
          cfg.workingDirectory;

      commonServiceConfig = {
        User = cfg.user;
        Group = cfg.group;
        WorkingDirectory = cfg.workingDirectory;

        Restart = cfg.restart;
        RestartSec = cfg.restartSec;

        UMask = "0007";

        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = false;
        ReadWritePaths = [
          cfg.stateDir
          cfg.workingDirectory
        ];
        PrivateTmp = true;
      };

      commonUnitEnvironment = {
        HOME = cfg.stateDir;
      }
      // common.processEnvironment { inherit icebathHome; }
      // common.backendSessionEnvironment { inherit cfg; };

      unitPath = common.processPath { inherit pkgs cfg; };

      lingerEnabled = ((config.users.users.${cfg.user} or { }).linger or false) == true;

    in
    {
      options.services.icebath-agent =
        common.sharedOptions {
          defaultPackage = icebath-agent;
          defaultPackageText = lib.literalExpression "icebath-agent.packages.\${system}.default";
          defaultWorkingDirectory = "${cfg.stateDir}/workspace";
          defaultWorkingDirectoryText = lib.literalExpression ''"''${cfg.stateDir}/workspace"'';
        }
        // (
          with lib;
          {
            user = mkOption {
              type = types.str;
              default = "icebath";
              description = "System user running the gateway.";
            };

            group = mkOption {
              type = types.str;
              default = "icebath";
              description = "System group running the gateway.";
            };

            createUser = mkOption {
              type = types.bool;
              default = true;
              description = "Create the user/group automatically.";
            };

            stateDir = mkOption {
              type = types.str;
              default = "/var/lib/icebath";
              description = "State directory. Contains .icebath/ subdir (ICEBATH_HOME).";
            };

            addToSystemPackages = mkOption {
              type = types.bool;
              default = false;
              description = ''
                Add the icebath CLI to environment.systemPackages and export
                ICEBATH_HOME system-wide (via environment.variables) so interactive
                shells share state with the gateway service.
              '';
            };

            container = {
              enable = mkEnableOption "OCI container mode (Ubuntu base, full self-modification support)";

              backend = mkOption {
                type = types.enum [
                  "docker"
                  "podman"
                ];
                default = "docker";
                description = "Container runtime.";
              };

              extraVolumes = mkOption {
                type = types.listOf types.str;
                default = [ ];
                description = "Extra volume mounts (host:container:mode format).";
                example = [ "/home/user/projects:/projects:rw" ];
              };

              extraOptions = mkOption {
                type = types.listOf types.str;
                default = [ ];
                description = "Extra arguments passed to docker/podman run.";
              };

              image = mkOption {
                type = types.str;
                default = "ubuntu:24.04";
                description = "OCI container image. The container pulls this at runtime via Docker/Podman.";
              };

              hostUsers = mkOption {
                type = types.listOf types.str;
                default = [ ];
                description = ''
                  Interactive users who get a ~/.icebath symlink to the service
                  stateDir. These users are automatically added to the icebath group.
                '';
                example = [ "sidbin" ];
              };
            };
          }
        );

      config = lib.mkIf cfg.enable (
        lib.mkMerge [

          (lib.mkIf (cfg.mcpServers != { }) {
            services.icebath-agent.settings.mcp_servers = common.mcpServersToConfig cfg.mcpServers;
          })

          (lib.mkIf cfg.createUser {
            users.groups.${cfg.group} = { };
            users.users.${cfg.user} = {
              isSystemUser = true;
              group = cfg.group;
              home = cfg.stateDir;
              createHome = true;
              shell = pkgs.bashInteractive;

              linger = lib.mkDefault (!cfg.container.enable);
            };
          })

          (lib.mkIf cfg.addToSystemPackages {
            environment.systemPackages = [ effectivePackage ];
            environment.variables.ICEBATH_HOME = icebathHome;
          })

          (lib.mkIf (cfg.container.enable && cfg.container.hostUsers != [ ]) {
            users.users = lib.genAttrs cfg.container.hostUsers (_user: {
              extraGroups = [ cfg.group ];
            });
          })

          {
            assertions =
              common.pluginNameAssertions {
                inherit cfg;
                optionPath = "services.icebath-agent";
              }
              ++ common.workspaceFilesAssertions {
                inherit cfg;
                opt = options.services.icebath-agent.workingDirectory;
                optionPath = "services.icebath-agent";
              }
              ++ common.backendBindAssertions {
                inherit cfg;
                optionPath = "services.icebath-agent";
              }
              ++ [
                {
                  assertion = !(cfg.container.enable && cfg.backend.mode != "none");
                  message = "services.icebath-agent: backend.mode is not supported together with container.enable — the container runs the gateway only.";
                }
              ];
          }

          (lib.mkIf (cfg.extraPackages != [ ]) {
            users.users.${cfg.user}.packages = cfg.extraPackages;
          })

          (lib.mkIf
            (cfg.container.enable && !cfg.addToSystemPackages && cfg.container.hostUsers != [ ])
            {
              warnings = [
                ''
                  services.icebath-agent: container.enable is true and container.hostUsers
                  is set, but addToSystemPackages is false. Without a host-installed icebath
                  binary, container routing will not work for interactive users.
                  Set addToSystemPackages = true or ensure icebath is on PATH.
                ''
              ];
            }
          )

          {
            systemd.tmpfiles.rules = [
              "d ${cfg.stateDir}                2770 ${cfg.user} ${cfg.group} - -"
              "d ${icebathHome}                  2770 ${cfg.user} ${cfg.group} - -"
              "d ${cfg.stateDir}/home           0750 ${cfg.user} ${cfg.group} - -"
              "d ${cfg.workingDirectory}        2770 ${cfg.user} ${cfg.group} - -"
            ]
            ++ map (d: "d ${icebathHome}/${d} 2770 ${cfg.user} ${cfg.group} - -") common.stateSubdirs;
          }

          {
            system.activationScripts."icebath-agent-setup" =
              lib.stringAfter
                (
                  [ "users" ] ++ lib.optional (config.system.activationScripts ? setupSecrets) "setupSecrets"
                )
                ''
                  mkdir -p ${icebathHome}
                  mkdir -p ${cfg.stateDir}/home
                  mkdir -p ${cfg.workingDirectory}
                  chown ${cfg.user}:${cfg.group} ${cfg.stateDir} ${icebathHome} ${cfg.stateDir}/home ${cfg.workingDirectory}
                  chmod 2770 ${cfg.stateDir} ${icebathHome} ${cfg.workingDirectory}
                  chmod 0750 ${cfg.stateDir}/home

                  find ${icebathHome} -maxdepth 1 \
                    \( -name "*.db" -o -name "*.db-wal" -o -name "*.db-shm" -o -name "SOUL.md" \) \
                    -exec chmod g+rw {} + 2>/dev/null || true
                  for _subdir in ${lib.concatStringsSep " " common.stateSubdirs}; do
                    mkdir -p "${icebathHome}/$_subdir"
                    chown ${cfg.user}:${cfg.group} "${icebathHome}/$_subdir"
                    chmod 2770 "${icebathHome}/$_subdir"
                    find "${icebathHome}/$_subdir" -type f \
                      -exec chmod g+rw {} + 2>/dev/null || true
                  done

                  ${common.mkStateScript {
                    inherit pkgs cfg icebathHome;
                    workingDirectory = cfg.workingDirectory;
                    configWorkingDirectory = effectiveWorkDir;
                    owner = "${cfg.user}:${cfg.group}";
                    stateDirs = common.stateSubdirs;
                    modes = {
                      config = configYamlMode;
                      env = "0640";
                      managed = "0644";
                      auth = "0600";
                      document = "0640";
                    };
                  }}

                  chown -h ${cfg.user}:${cfg.group} ${icebathHome}/plugins/nix-managed-* 2>/dev/null || true

                  ${
                    if cfg.container.enable then
                      ''
                        install -o ${cfg.user} -g ${cfg.group} -m 0644 ${containerModeFile} ${icebathHome}/.container-mode
                      ''
                    else
                      ''
                        rm -f ${icebathHome}/.container-mode

                        ${lib.concatStringsSep "\n" (
                          map (
                            user:
                            let
                              userHome = config.users.users.${user}.home;
                              symlinkPath = "${userHome}/.icebath";
                            in
                            ''
                              if [ -L "${symlinkPath}" ] && [ "$(readlink "${symlinkPath}")" = "${icebathHome}" ]; then
                                rm -f "${symlinkPath}"
                                echo "icebath-agent: removed symlink ${symlinkPath}"
                              fi
                            ''
                          ) cfg.container.hostUsers
                        )}
                      ''
                  }

                  ${lib.optionalString cfg.container.enable (
                    lib.concatStringsSep "\n" (
                      map (
                        user:
                        let
                          userHome = config.users.users.${user}.home;
                          symlinkPath = "${userHome}/.icebath";
                        in
                        ''
                          if [ -d "${symlinkPath}" ] && [ ! -L "${symlinkPath}" ]; then
                            _backup="${symlinkPath}.bak.$(date +%s)"
                            echo "icebath-agent: backing up existing ${symlinkPath} to $_backup"
                            mv "${symlinkPath}" "$_backup"
                          fi
                          ln -sfn "${icebathHome}" "${symlinkPath}"
                          chown -h ${user}:${cfg.group} "${symlinkPath}"
                        ''
                      ) cfg.container.hostUsers
                    )
                  )}
                '';
          }

          (lib.mkIf (!cfg.container.enable) {
            systemd.services.icebath-agent = {
              description = "Icebath Agent Gateway";
              wantedBy = [ "multi-user.target" ];
              after = [
                "network-online.target"
              ]
              ++ lib.optional lingerEnabled "linger-users.service";
              wants = [
                "network-online.target"
              ]
              ++ lib.optional lingerEnabled "linger-users.service";

              environment = commonUnitEnvironment;

              preStart = lib.mkIf lingerEnabled ''
                for _ in $(seq 1 50); do
                  [ -S "/run/user/$(id -u)/bus" ] && break
                  sleep 0.2
                done
                if [ ! -S "/run/user/$(id -u)/bus" ]; then
                  echo "icebath-agent: no user bus at /run/user/$(id -u)/bus after 10s;" \
                       "restart-safe cron dispatch will fail for the life of this process" >&2
                fi
              '';

              serviceConfig = commonServiceConfig // {
                ExecStart = lib.escapeShellArgs (common.gatewayArgv cfg);
              };

              path = unitPath;
            };
          })

          (lib.mkIf (!cfg.container.enable && cfg.backend.mode != "none") {
            systemd.services.icebath-backend = {
              description = common.backendDescription cfg;
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];

              environment = commonUnitEnvironment;

              serviceConfig = commonServiceConfig // {
                ExecStart = lib.escapeShellArgs (common.backendArgv { inherit pkgs cfg; });
              };

              path = unitPath;
            };
          })

          (lib.mkIf cfg.container.enable {
            virtualisation.docker.enable = lib.mkDefault (cfg.container.backend == "docker");

            systemd.services.icebath-agent = {
              description = "Icebath Agent Gateway (container)";
              wantedBy = [ "multi-user.target" ];
              after = [
                "network-online.target"
              ]
              ++ lib.optional (cfg.container.backend == "docker") "docker.service";
              wants = [ "network-online.target" ];
              requires = lib.optional (cfg.container.backend == "docker") "docker.service";

              preStart = ''
                ln -sfn ${effectivePackage} ${cfg.stateDir}/current-package
                ln -sfn ${containerEntrypoint} ${cfg.stateDir}/current-entrypoint

                ${pkgs.nix}/bin/nix-store --add-root ${cfg.stateDir}/.gc-root --indirect -r ${effectivePackage} 2>/dev/null || true
                ${pkgs.nix}/bin/nix-store --add-root ${cfg.stateDir}/.gc-root-entrypoint --indirect -r ${containerEntrypoint} 2>/dev/null || true

                NEED_CREATE=false
                if ! ${containerBin} inspect ${containerName} &>/dev/null; then
                  NEED_CREATE=true
                elif [ ! -f ${identityFile} ] || [ "$(cat ${identityFile})" != "${containerIdentity}" ]; then
                  echo "Container config changed, recreating..."
                  ${containerBin} rm -f ${containerName} || true
                  NEED_CREATE=true
                fi

                if [ "$NEED_CREATE" = "true" ]; then
                  ICEBATH_UID=$(${pkgs.coreutils}/bin/id -u ${cfg.user})
                  ICEBATH_GID=$(${pkgs.coreutils}/bin/id -g ${cfg.user})

                  echo "Creating container..."
                  ${containerBin} create \
                    --name ${containerName} \
                    --network=host \
                    --entrypoint ${containerDataDir}/current-entrypoint \
                    --volume /nix/store:/nix/store:ro \
                    --volume ${cfg.stateDir}:${containerDataDir} \
                    --volume ${cfg.stateDir}/home:${containerHomeDir} \
                    ${lib.concatStringsSep " " (map (v: "--volume ${v}") cfg.container.extraVolumes)} \
                    --env ICEBATH_UID="$ICEBATH_UID" \
                    --env ICEBATH_GID="$ICEBATH_GID" \
                    --env ICEBATH_HOME=${containerDataDir}/.icebath \
                    --env ICEBATH_MANAGED=true \
                    --env HOME=${containerHomeDir} \
                    ${lib.concatStringsSep " " cfg.container.extraOptions} \
                    ${cfg.container.image} \
                    ${containerDataDir}/current-package/bin/icebath gateway run --replace ${lib.concatStringsSep " " cfg.extraArgs}

                  echo "${containerIdentity}" > ${identityFile}
                fi
              '';

              script = ''
                exec ${containerBin} start -a ${containerName}
              '';

              preStop = ''
                ${containerBin} stop -t 10 ${containerName} || true
              '';

              serviceConfig = {
                Type = "simple";
                Restart = cfg.restart;
                RestartSec = cfg.restartSec;
                TimeoutStopSec = 30;
              };
            };
          })
        ]
      );
    };
}
