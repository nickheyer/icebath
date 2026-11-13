# Home Manager module installing icebath and running its gateway and backend as user services
{ inputs, ... }:
{
  flake.homeManagerModules.default =
    {
      config,
      lib,
      options,
      pkgs,
      ...
    }:

    let
      cfg = config.services.icebath-agent;
      cfgPrograms = config.programs.icebath-agent;
      common = import ./moduleCommon.nix { inherit lib; };

      effectivePackage = common.effectivePackage cfg;
      icebath-agent = inputs.self.packages.${pkgs.stdenv.hostPlatform.system}.default;

      inherit (pkgs.stdenv.hostPlatform) isDarwin isLinux;

      processEnvironment = common.processEnvironment {
        inherit (cfg) icebathHome;
        managedSystem = "home-manager";
      };
      unitPath = lib.makeBinPath (common.processPath { inherit pkgs cfg; });

      mkUnit =
        {
          description,
          argv,
          environment ? { },
        }:
        {
          Unit = {
            Description = description;
            After = [ "default.target" ];
          };
          Install.WantedBy = [ "default.target" ];
          Service = {
            Type = "simple";
            Environment =
              (lib.mapAttrsToList (k: v: "${k}=${v}") processEnvironment)
              ++ (lib.mapAttrsToList (k: v: "${k}=${lib.escapeShellArg v}") environment)
              ++ [
                "PATH=${unitPath}"
              ];
            ExecStart = lib.escapeShellArgs argv;
            WorkingDirectory = cfg.workingDirectory;
            Restart = cfg.restart;
            RestartSec = cfg.restartSec;
            UMask = "0077";
            NoNewPrivileges = true;
            PrivateTmp = true;
          };
        };

      mkAgent =
        {
          argv,
          logName,
          environment ? { },
        }:
        {
          enable = true;
          config = {
            Label = "org.nix-community.home.${logName}";
            ProgramArguments = argv;
            EnvironmentVariables =
              processEnvironment
              // environment
              // {
                PATH = "${unitPath}:/usr/bin:/bin:/usr/sbin:/sbin";
              };
            WorkingDirectory = cfg.workingDirectory;
            RunAtLoad = true;
            KeepAlive =
              if cfg.restart == "always" then
                true
              else
                {
                  SuccessfulExit = false;
                  Crashed = true;
                };
            ThrottleInterval = cfg.restartSec;
            StandardOutPath = "${config.home.homeDirectory}/Library/Logs/${logName}.log";
            StandardErrorPath = "${config.home.homeDirectory}/Library/Logs/${logName}.err.log";
            ProcessType = "Background";
          };
        };

    in
    {
      options.programs.icebath-agent = {
        enable = lib.mkEnableOption ''
          the Icebath Agent command line application.

          This adds `icebath` to home.packages, and exports ICEBATH_HOME with
          home.sessionVariables. An interactive shell then uses the same
          state as `services.icebath-agent`
        '';

        package = lib.mkOption {
          type = lib.types.package;
          default = effectivePackage;
          defaultText = lib.literalExpression "config.services.icebath-agent.package";
          description = ''
            The icebath-agent package to install.

            The default follows `services.icebath-agent.package`, and applies
            `extraPythonPackages` from that module. Thus the command line
            and the services are one build, and a plugin that the services
            can load is a plugin that your shell can load.
          '';
        };
      };

      options.services.icebath-agent =
        common.sharedOptions {
          defaultPackage = icebath-agent;
          defaultPackageText = lib.literalExpression "icebath-agent.packages.\${system}.default";
          defaultWorkingDirectory = config.home.homeDirectory;
          defaultWorkingDirectoryText = lib.literalExpression "config.home.homeDirectory";
        }
        // {
          icebathHome = lib.mkOption {
            type = lib.types.str;
            default = "${config.home.homeDirectory}/.icebath";
            defaultText = lib.literalExpression ''"''${config.home.homeDirectory}/.icebath"'';
            description = ''
              The value of ICEBATH_HOME. This state directory holds
              config.yaml, .env, auth.json, the sessions, the skills, the
              memory and the cron jobs.

              The NixOS module takes a `stateDir` and adds `/.icebath` to it.
              This module sets ICEBATH_HOME directly. Thus an existing
              ~/.icebath continues to work, and you can give the directory any
              name.
            '';
            example = "/home/alice/.icebath-work";
          };

          installPackage = lib.mkOption {
            type = lib.types.nullOr lib.types.bool;
            default = null;
            visible = false;
            description = ''
              Removed. Use `programs.icebath-agent.enable` instead.
            '';
          };

          gateway.enable = lib.mkEnableOption "the messaging gateway service (Telegram, Discord, Slack, ...)";
        };

      config = lib.mkMerge [

        (lib.mkIf cfgPrograms.enable {
          home.packages = [ cfgPrograms.package ];
          home.sessionVariables.ICEBATH_HOME = cfg.icebathHome;
        })

        {
          assertions = [
            {
              assertion = cfg.installPackage == null;
              message = common.installPackageRemovedMessage cfg.installPackage;
            }
          ];
        }

        (lib.mkIf cfg.enable (
          lib.mkMerge [

            (lib.mkIf (cfg.mcpServers != { }) {
              services.icebath-agent.settings.mcp_servers = common.mcpServersToConfig cfg.mcpServers;
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
                    assertion = !isDarwin || cfg.backend.waitFor != "interface";
                    message = "services.icebath-agent.backend.waitFor = \"interface\" works on Linux only. Use \"hostname\" on Darwin.";
                  }
                ];
            }

            (lib.mkIf cfgPrograms.enable {
              home.packages = cfg.extraPackages;
            })

            {
              home.activation.icebathAgentSetup =
                lib.hm.dag.entryAfter
                  [
                    "writeBoundary"
                    "linkGeneration"
                  ]
                  (
                    common.mkStateScript {
                      inherit pkgs cfg;
                      inherit (cfg) icebathHome workingDirectory;
                      run = "$DRY_RUN_CMD ";
                      stateDirs = common.stateSubdirs;
                      managedSystem = "home-manager";
                      modes = {
                        config = "0600";
                        env = "0600";
                        managed = "0600";
                        auth = "0600";
                        document = "0600";
                      };
                    }
                  );
            }

            (lib.mkIf (isLinux && cfg.gateway.enable) {
              systemd.user.services.icebath-agent = mkUnit {
                description = "Icebath Agent Gateway";
                argv = common.gatewayArgv cfg;
              };
            })

            (lib.mkIf (isLinux && cfg.backend.mode != "none") {
              systemd.user.services.icebath-backend = mkUnit {
                description = common.backendDescription cfg;
                argv = common.backendArgv { inherit pkgs cfg; };
                environment = common.backendSessionEnvironment { inherit cfg; };
              };
            })

            (lib.mkIf (isDarwin && cfg.gateway.enable) {
              launchd.agents.icebath-agent = mkAgent {
                argv = common.gatewayArgv cfg;
                logName = "icebath-agent";
              };
            })

            (lib.mkIf (isDarwin && cfg.backend.mode != "none") {
              launchd.agents.icebath-backend = mkAgent {
                argv = common.backendArgv { inherit pkgs cfg; };
                logName = "icebath-backend";
                environment = common.backendSessionEnvironment { inherit cfg; };
              };
            })
          ]
        ))
      ];
    };
}
