# Options, config renderers and state setup that the NixOS and Home Manager modules share
{ lib }:

let
  inherit (lib)
    literalExpression
    mkOption
    types
    ;

  deepConfigType = types.mkOptionType {
    name = "icebath-config-attrs";
    description = "Icebath YAML config (attrset), merged deeply via lib.recursiveUpdate.";
    check = builtins.isAttrs;
    merge = _loc: defs: lib.foldl' lib.recursiveUpdate { } (map (d: d.value) defs);
  };

  mcpServerType = types.submodule {
    options = {
      command = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "MCP server command (stdio transport).";
      };
      args = mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = "Command-line arguments (stdio transport).";
      };
      env = mkOption {
        type = types.attrsOf types.str;
        default = { };
        description = "Environment variables for the server process (stdio transport).";
      };

      url = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "MCP server endpoint URL (HTTP/StreamableHTTP transport).";
      };
      headers = mkOption {
        type = types.attrsOf types.str;
        default = { };
        description = "HTTP headers, e.g. for authentication (HTTP transport).";
      };

      auth = mkOption {
        type = types.nullOr (types.enum [ "oauth" ]);
        default = null;
        description = ''
          Authentication method. Set to "oauth" for OAuth 2.1 PKCE flow
          (remote MCP servers). Tokens are stored in $ICEBATH_HOME/mcp-tokens/.
        '';
      };

      enabled = mkOption {
        type = types.bool;
        default = true;
        description = "Enable or disable this MCP server.";
      };

      timeout = mkOption {
        type = types.nullOr types.int;
        default = null;
        description = "Tool call timeout in seconds (default: 120).";
      };
      connect_timeout = mkOption {
        type = types.nullOr types.int;
        default = null;
        description = "Initial connection timeout in seconds (default: 60).";
      };

      tools = mkOption {
        type = types.nullOr (
          types.submodule {
            options = {
              include = mkOption {
                type = types.listOf types.str;
                default = [ ];
                description = "Tool allowlist — only these tools are registered.";
              };
              exclude = mkOption {
                type = types.listOf types.str;
                default = [ ];
                description = "Tool blocklist — these tools are hidden.";
              };
            };
          }
        );
        default = null;
        description = "Filter which tools are exposed by this server.";
      };

      sampling = mkOption {
        type = types.nullOr (
          types.submodule {
            options = {
              enabled = mkOption {
                type = types.bool;
                default = true;
                description = "Enable sampling.";
              };
              model = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "Override model for sampling requests.";
              };
              max_tokens_cap = mkOption {
                type = types.nullOr types.int;
                default = null;
                description = "Max tokens per request.";
              };
              timeout = mkOption {
                type = types.nullOr types.int;
                default = null;
                description = "LLM call timeout in seconds.";
              };
              max_rpm = mkOption {
                type = types.nullOr types.int;
                default = null;
                description = "Max requests per minute.";
              };
              max_tool_rounds = mkOption {
                type = types.nullOr types.int;
                default = null;
                description = "Max tool-use rounds per sampling request.";
              };
              allowed_models = mkOption {
                type = types.listOf types.str;
                default = [ ];
                description = "Models the server is allowed to request.";
              };
              log_level = mkOption {
                type = types.nullOr (
                  types.enum [
                    "debug"
                    "info"
                    "warning"
                  ]
                );
                default = null;
                description = "Audit log level for sampling requests.";
              };
            };
          }
        );
        default = null;
        description = "Sampling configuration for server-initiated LLM requests.";
      };
    };
  };

  mcpServersToConfig =
    servers:
    lib.mapAttrs (
      _name: srv:
      lib.optionalAttrs (srv.command != null) { inherit (srv) command args; }
      // lib.optionalAttrs (srv.env != { }) { inherit (srv) env; }
      // lib.optionalAttrs (srv.url != null) { inherit (srv) url; }
      // lib.optionalAttrs (srv.headers != { }) { inherit (srv) headers; }
      // lib.optionalAttrs (srv.auth != null) { inherit (srv) auth; }
      // {
        inherit (srv) enabled;
      }
      // lib.optionalAttrs (srv.timeout != null) { inherit (srv) timeout; }
      // lib.optionalAttrs (srv.connect_timeout != null) { inherit (srv) connect_timeout; }
      // lib.optionalAttrs (srv.tools != null) {
        tools = lib.filterAttrs (_: v: v != [ ]) {
          inherit (srv.tools) include exclude;
        };
      }
      // lib.optionalAttrs (srv.sampling != null) {
        sampling = lib.filterAttrs (_: v: v != null && v != [ ]) {
          inherit (srv.sampling)
            enabled
            model
            max_tokens_cap
            timeout
            max_rpm
            max_tool_rounds
            allowed_models
            log_level
            ;
        };
      }
    ) servers;

  documentsType = types.attrsOf (types.either types.str types.path);

  sharedOptions =
    {
      defaultPackage,
      defaultPackageText,
      defaultWorkingDirectory,
      defaultWorkingDirectoryText,
    }:
    {
      enable = lib.mkEnableOption "Icebath Agent";

      package = mkOption {
        type = types.package;
        default = defaultPackage;
        defaultText = defaultPackageText;
        description = "The icebath-agent package to use.";
      };

      workingDirectory = mkOption {
        type = types.str;
        default = defaultWorkingDirectory;
        defaultText = defaultWorkingDirectoryText;
        description = ''
          The working directory for the agent. The module also writes this
          path to config.yaml as `terminal.cwd`. The terminal and file tools
          of the agent use that value.
        '';
      };

      configFile = mkOption {
        type = types.nullOr types.path;
        default = null;
        description = ''
          The path to an existing config.yaml. If you set this option, it
          replaces the `settings` option. The module installs the file
          without a change and overwrites all runtime edits on each
          activation.
        '';
      };

      settings = mkOption {
        type = deepConfigType;
        default = { };
        description = ''
          The Icebath configuration, as an attribute set. The module joins the
          definitions from all modules and writes the result to config.yaml.

          The merge into the config.yaml on disk is also a deep merge. These
          keys replace the keys on disk. The module keeps all other keys,
          which includes the keys that `icebath config set` and the settings
          panes of the TUI write at runtime.
        '';
        example = literalExpression ''
          {
            model.default = "anthropic/claude-sonnet-4";
            terminal.backend = "local";
            compression = { enabled = true; threshold = 0.85; };
          }
        '';
      };

      environmentFiles = mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = ''
          The paths to environment files that contain secrets, for example
          API keys and tokens. Activation adds the contents of these files to
          $ICEBATH_HOME/.env. Icebath reads that file at each start, with
          load_icebath_dotenv().

          Each activation writes .env again from the start. Thus a secret
          file cannot go into .env two times.
        '';
        example = literalExpression ''[ config.sops.secrets."icebath/env".path ]'';
      };

      environment = mkOption {
        type = types.attrsOf types.str;
        default = { };
        description = ''
          Environment variables that are not secret. Activation writes them
          to $ICEBATH_HOME/.env.

          CAUTION: Do not put secrets in this option. All users can read the
          Nix store. Use environmentFiles for secrets.
        '';
      };

      authFile = mkOption {
        type = types.nullOr types.path;
        default = null;
        description = ''
          The path to a file that gives the first contents of auth.json, the
          OAuth credentials. The module copies the file only when auth.json
          does not exist. Thus a token that Icebath refreshes at runtime stays
          after an activation.
        '';
      };

      authFileForceOverwrite = mkOption {
        type = types.bool;
        default = false;
        description = "Always overwrite auth.json from authFile on activation.";
      };

      documents = mkOption {
        type = documentsType;
        default = { };
        description = ''
          Workspace files. The module installs them into workingDirectory.
          Each key is a path relative to that directory, and the module makes
          the necessary subdirectories. Each value is a string or a path.

          Use this option for the project context that the agent reads from
          its working directory, for example AGENTS.md, notes and checklists.
          Icebath reads SOUL.md and memories/ from ICEBATH_HOME, so put those
          files in `icebathHomeFiles`.

          If you set this option, you must also set `workingDirectory`. The
          default of that option is different on each module. Thus an unset
          default puts these files in a directory that you did not select.
        '';
        example = literalExpression ''
          {
            "AGENTS.md" = ./AGENTS.md;
            "notes/oncall.md" = "Page #infra before restarting anything.";
          }
        '';
      };

      icebathHomeFiles = mkOption {
        type = documentsType;
        default = { };
        description = ''
          Files that the module installs into ICEBATH_HOME. Each key is a path
          relative to that directory, and the module makes the necessary
          subdirectories. Each value is a string or a path.

          Icebath reads SOUL.md and the memory files from ICEBATH_HOME and not
          from the working directory. Declare those files here, or Icebath
          does not load them.
        '';
        example = literalExpression ''
          {
            "SOUL.md" = "You are a helpful AI assistant.";
            "memories/USER.md" = ./USER.md;
          }
        '';
      };

      mcpServers = mkOption {
        type = types.attrsOf mcpServerType;
        default = { };
        description = ''
          MCP server configurations (merged into settings.mcp_servers).
          Each server uses either stdio (command/args) or HTTP (url) transport.
        '';
        example = literalExpression ''
          {
            filesystem = {
              command = "npx";
              args = [ "-y" "@modelcontextprotocol/server-filesystem" "/home/user" ];
            };
            remote-api = {
              url = "http://my-server:8080/v0/mcp";
              headers = { Authorization = "Bearer ..."; };
            };
            remote-oauth = {
              url = "https://mcp.example.com/mcp";
              auth = "oauth";
            };
          }
        '';
      };

      extraPackages = mkOption {
        type = types.listOf types.package;
        default = [ ];
        description = "More packages on the PATH of the agent. The agent can run these tools.";
      };

      extraPlugins = mkOption {
        type = types.listOf types.package;
        default = [ ];
        description = ''
          Directory-based plugin packages to symlink into the icebath plugins
          directory. Each package must contain a plugin.yaml and __init__.py
          at its root. Icebath discovers these automatically on startup.
        '';
        example = literalExpression ''
          [
            (pkgs.fetchFromGitHub {
              owner = "stephenschoettler";
              repo = "hermes-lcm";
              name = "hermes-lcm";
              rev = "v0.7.0";
              hash = "sha256-...";
            })
          ]
        '';
      };

      extraPythonPackages = mkOption {
        type = types.listOf types.package;
        default = [ ];
        description = ''
          Python packages to add to PYTHONPATH for entry-point plugin discovery.
          These are pip-packaged plugins that register via the
          icebath_agent.plugins entry-point group. The plugin host of icebath
          runs them with its managed Python interpreter, so each package must
          be built for that interpreter. Its major.minor is derived from
          pkg/pm/lock.json by nix/pythonLock.nix — take packages from
          config.services.icebath-agent.package.python.pkgs so the set always
          matches the interpreter the plugin host runs.
        '';
        example = literalExpression ''
          [
            (config.services.icebath-agent.package.python.pkgs.buildPythonPackage {
              pname = "rtk-hermes";
              version = "1.0.0";
              src = pkgs.fetchFromGitHub {
                owner = "ogallotti";
                repo = "rtk-hermes";
                rev = "main";
                hash = "sha256-...";
              };
            })
          ]
        '';
      };

      extraArgs = mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = "Extra command-line arguments for `icebath gateway`.";
      };

      restart = mkOption {
        type = types.str;
        default = "always";
        description = "The systemd Restart= policy. Darwin does not use this option.";
      };

      restartSec = mkOption {
        type = types.int;
        default = 5;
        description = "The systemd RestartSec= value. Darwin does not use this option.";
      };
    };

  installPackageRemovedMessage =
    value:
    ''
      services.icebath-agent.installPackage was removed. Icebath now
      separates the installation from the services, which is the
      Home Manager convention:

        programs.icebath-agent.enable = ${lib.boolToString (value != false)};  # the icebath CLI, and ICEBATH_HOME for your shells

      `services.icebath-agent` keeps the state, the configuration and
      the daemons. Remove `installPackage` and add the line above.
    '';

  effectivePackage =
    cfg:
    if cfg.extraPythonPackages == [ ] then
      cfg.package
    else
      cfg.package.override (prev: {
        extraPythonPackages = (prev.extraPythonPackages or [ ]) ++ cfg.extraPythonPackages;
      });

  mkConfigFiles =
    {
      pkgs,
      cfg,
      workingDirectory,
    }:
    let
      generated =
        pkgs.runCommand "icebath-config.yaml"
          {
            settings = builtins.toJSON (lib.recursiveUpdate { terminal.cwd = workingDirectory; } cfg.settings);
            passAsFile = [ "settings" ];
          }
          ''
            HOME=$TMPDIR ${effectivePackage cfg}/bin/icebath nix-config-render "$settingsPath" > $out
          '';
    in
    {
      inherit generated;
      effective = if cfg.configFile != null then cfg.configFile else generated;
      mergeScript = "${effectivePackage cfg}/bin/icebath nix-config-merge";
    };

  mkDocumentTree =
    { pkgs, documents }:
    pkgs.runCommand "icebath-documents" { } (
      ''
        mkdir -p $out
      ''
      + lib.concatStringsSep "\n" (
        lib.mapAttrsToList (
          name: value:
          let
            dir = builtins.dirOf name;
            mkdir = lib.optionalString (dir != ".") "mkdir -p $out/${dir}";
          in
          if builtins.isPath value || lib.isStorePath value then
            "${mkdir}\ncp ${value} $out/${name}"
          else
            "${mkdir}\ncat > $out/${name} <<'ICEBATH_DOC_EOF'\n${value}\nICEBATH_DOC_EOF"
        ) documents
      )
    );

  mkEnvScript =
    { pkgs, environment }:
    let
      base = pkgs.writeText "icebath-env-base" (
        lib.concatStringsSep "\n" (lib.mapAttrsToList (k: v: "${k}=${v}") environment)
        + lib.optionalString (environment != { }) "\n"
      );
    in
    pkgs.writeShellScript "icebath-env-merge" ''
      set -eu

      dest="$1"
      mode="$2"
      shift 2

      install -m "$mode" ${base} "$dest"
      for file in "$@"; do
        if [ -r "$file" ]; then
          printf '\n' >> "$dest"
          cat "$file" >> "$dest"
        else
          echo "icebath-agent: WARNING cannot read environmentFile $file" >&2
        fi
      done
    '';

  mkStateScript =
    {
      pkgs,
      cfg,
      icebathHome,
      workingDirectory,
      configWorkingDirectory ? workingDirectory,
      run ? "",
      owner ? null,
      modes,
      stateDirs ? [ ],
      managedSystem ? "nixos",
    }:
    let
      installFlags = lib.optionalString (owner != null) (
        let
          parts = lib.splitString ":" owner;
        in
        "-o ${lib.head parts} -g ${lib.last parts}"
      );
      configFiles = mkConfigFiles {
        inherit pkgs cfg;
        workingDirectory = configWorkingDirectory;
      };
      envScript = mkEnvScript {
        inherit pkgs;
        inherit (cfg) environment;
      };
      documentTree = mkDocumentTree {
        inherit pkgs;
        inherit (cfg) documents;
      };
      homeDocumentTree = mkDocumentTree {
        inherit pkgs;
        documents = cfg.icebathHomeFiles;
      };

      inst = "${run}install ${installFlags}";

      installDocuments =
        tree: root: docs:
        lib.concatStringsSep "\n" (
          lib.mapAttrsToList (
            name: _value: "${inst} -m ${modes.document} -D ${tree}/${name} ${root}/${name}"
          ) docs
        );
    in
    ''
      ${run}mkdir -p ${
        lib.escapeShellArgs (
          [
            icebathHome
            workingDirectory
          ]
          ++ map (d: "${icebathHome}/${d}") stateDirs
        )
      }

      ${
        if cfg.configFile != null then
          "${inst} -m ${modes.config} -D ${configFiles.effective} ${icebathHome}/config.yaml"
        else
          ''
            ${run}${configFiles.mergeScript} ${configFiles.generated} ${icebathHome}/config.yaml
            ${run}chmod ${modes.config} ${icebathHome}/config.yaml
          ''
      }

      ${inst} -m ${modes.managed} ${pkgs.writeText "icebath-managed" managedSystem} ${icebathHome}/.managed

      ${lib.optionalString (cfg.environment != { } || cfg.environmentFiles != [ ]) ''
        ${run}${envScript} ${icebathHome}/.env ${modes.env} ${lib.escapeShellArgs cfg.environmentFiles}
        ${lib.optionalString (owner != null) "${run}chown ${owner} ${icebathHome}/.env"}
      ''}

      ${lib.optionalString (cfg.authFile != null) (
        if cfg.authFileForceOverwrite then
          "${inst} -m ${modes.auth} ${cfg.authFile} ${icebathHome}/auth.json"
        else
          ''
            if [ ! -e ${icebathHome}/auth.json ]; then
              ${inst} -m ${modes.auth} ${cfg.authFile} ${icebathHome}/auth.json
            fi
          ''
      )}

      ${installDocuments documentTree workingDirectory cfg.documents}
      ${installDocuments homeDocumentTree icebathHome cfg.icebathHomeFiles}

      ${run}find ${icebathHome}/plugins -maxdepth 1 -type l -name 'nix-managed-*' -delete 2>/dev/null || true
      ${lib.concatMapStringsSep "\n" (plugin: ''
        if [ ! -f ${plugin}/plugin.yaml ]; then
          echo "icebath-agent: ERROR extraPlugins entry '${plugin}' has no plugin.yaml" >&2
          exit 1
        fi
        ${run}ln -sfn ${plugin} ${icebathHome}/plugins/nix-managed-${lib.getName plugin}
      '') cfg.extraPlugins}
    '';

  gatewayArgv =
    cfg:
    [
      "${effectivePackage cfg}/bin/icebath"
      "gateway"
    ]
    ++ cfg.extraArgs;

  processEnvironment =
    {
      icebathHome,
      managedSystem ? "true",
    }:
    {
      ICEBATH_HOME = icebathHome;
      ICEBATH_MANAGED = managedSystem;
    };

  processPath =
    { pkgs, cfg }:
    [
      (effectivePackage cfg)
      pkgs.bash
      pkgs.coreutils
      pkgs.git
    ]
    ++ cfg.extraPackages;

  workspaceFilesAssertions =
    {
      cfg,
      opt,
      optionPath,
    }:
    let
      untouched = (lib.mkOptionDefault null).priority;
    in
    [
      {
        assertion = cfg.documents == { } || opt.highestPrio < untouched;
        message = ''
          ${optionPath}.documents needs an explicit ${optionPath}.workingDirectory.

          The files go into workingDirectory. The default of that option is
          different on each module, so an unset default puts the files in a
          directory that you did not select. Set the directory:

            ${optionPath}.workingDirectory = "/path/you/want";

          To give Icebath an identity and a memory, use
          ${optionPath}.icebathHomeFiles instead. Those files go to
          ICEBATH_HOME. Icebath reads SOUL.md and memories/ only from there.
        '';
      }
    ];

  pluginNameAssertions =
    { cfg, optionPath }:
    let
      names = map lib.getName cfg.extraPlugins;
    in
    [
      {
        assertion = (lib.length names) == (lib.length (lib.unique names));
        message = "${optionPath}.extraPlugins: duplicate plugin names detected: ${toString names}. If using fetchFromGitHub, set name = \"plugin-name\" to disambiguate.";
      }
    ];

  stateSubdirs = [
    "cron"
    "sessions"
    "logs"
    "memories"
    "plugins"
  ];
in
{
  inherit
    deepConfigType
    effectivePackage
    gatewayArgv
    installPackageRemovedMessage
    mcpServerType
    mcpServersToConfig
    mkConfigFiles
    mkDocumentTree
    mkEnvScript
    mkStateScript
    pluginNameAssertions
    processEnvironment
    processPath
    sharedOptions
    stateSubdirs
    workspaceFilesAssertions
    ;
}
