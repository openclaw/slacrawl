package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	commandNames = []string{
		"init",
		"version",
		"check-update",
		"metadata",
		"doctor",
		"report",
		"digest",
		"analytics",
		"publish",
		"subscribe",
		"update",
		"sync",
		"import",
		"purge",
		"tail",
		"watch",
		"tui",
		"search",
		"messages",
		"files",
		"mentions",
		"sql",
		"users",
		"channels",
		"status",
		"completion",
	}
	globalFlags = []string{
		"--config",
		"--format",
		"--json",
		"--no-color",
		"--help",
		"-h",
	}
)

func (a *App) runCompletion(args []string) error {
	if len(args) == 0 {
		return errors.New("completion shell required: bash or zsh")
	}
	shell := strings.ToLower(strings.TrimSpace(args[0]))
	switch shell {
	case "bash":
		_, err := io.WriteString(a.Stdout, bashCompletionScript())
		return err
	case "zsh":
		_, err := io.WriteString(a.Stdout, zshCompletionScript())
		return err
	default:
		return fmt.Errorf("unsupported completion shell %q: use bash or zsh", shell)
	}
}

func bashCompletionScript() string {
	commands := strings.Join(commandNames, " ")
	global := strings.Join(globalFlags, " ")
	var b strings.Builder
	b.WriteString(`# bash completion for slacrawl
_slacrawl()
{
    local cur prev words cword
    _init_completion || return

    local commands="`)
	b.WriteString(commands)
	b.WriteString(`"
    local global_flags="`)
	b.WriteString(global)
	b.WriteString(`"

    if [[ ${cword} -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "${commands} ${global_flags}" -- "${cur}") )
        return
    fi

    local command=""
    local i
    for ((i=1; i < ${#words[@]}; i++)); do
        case "${words[i]}" in
            init|version|check-update|metadata|doctor|report|digest|analytics|publish|subscribe|update|sync|import|purge|tail|watch|tui|search|messages|files|mentions|sql|users|channels|status|completion)
                command="${words[i]}"
                break
                ;;
        esac
    done

	case "${prev}" in
		--format)
			COMPREPLY=( $(compgen -W "text json log" -- "${cur}") )
			return
			;;
		--source)
			COMPREPLY=( $(compgen -W "api bot desktop wiretap mcp connector all" -- "${cur}") )
			return
			;;
		--kind)
			COMPREPLY=( $(compgen -W "im mpim public_channel private_channel" -- "${cur}") )
			return
			;;
		completion)
			COMPREPLY=( $(compgen -W "bash zsh" -- "${cur}") )
			return
			;;
		analytics)
			COMPREPLY=( $(compgen -W "digest quiet trends --help -h ${global_flags}" -- "${cur}") )
			return
			;;
    esac

    case "${command}" in
        init)
            COMPREPLY=( $(compgen -W "--workspace --db --help -h ${global_flags}" -- "${cur}") )
            ;;
        version)
            COMPREPLY=( $(compgen -W "--help -h ${global_flags}" -- "${cur}") )
            ;;
        check-update)
            COMPREPLY=( $(compgen -W "--json --help -h ${global_flags}" -- "${cur}") )
            ;;
        metadata)
            COMPREPLY=( $(compgen -W "--json --help -h ${global_flags}" -- "${cur}") )
            ;;
        doctor)
            COMPREPLY=( $(compgen -W "--help -h ${global_flags}" -- "${cur}") )
            ;;
        report)
            COMPREPLY=( $(compgen -W "--help -h ${global_flags}" -- "${cur}") )
            ;;
        digest)
            COMPREPLY=( $(compgen -W "--since --workspace --channel --top-n --help -h ${global_flags}" -- "${cur}") )
            ;;
        analytics)
            local analytics_subcommand=""
            for ((i=2; i < ${#words[@]}; i++)); do
                case "${words[i]}" in
                    digest|quiet|trends)
                        analytics_subcommand="${words[i]}"
                        break
                        ;;
                esac
            done
            case "${analytics_subcommand}" in
                digest)
                    COMPREPLY=( $(compgen -W "--since --workspace --channel --top-n --help -h ${global_flags}" -- "${cur}") )
                    ;;
                quiet)
                    COMPREPLY=( $(compgen -W "--since --workspace --format --json --help -h ${global_flags}" -- "${cur}") )
                    ;;
                trends)
                    COMPREPLY=( $(compgen -W "--weeks --workspace --channel --format --json --help -h ${global_flags}" -- "${cur}") )
                    ;;
                *)
                    COMPREPLY=( $(compgen -W "digest quiet trends --help -h ${global_flags}" -- "${cur}") )
                    ;;
            esac
            ;;
        publish)
            COMPREPLY=( $(compgen -W "--repo --remote --branch --message --tag --no-commit --push --no-media --help -h ${global_flags}" -- "${cur}") )
            ;;
        subscribe)
            COMPREPLY=( $(compgen -W "--repo --db --remote --branch --stale-after --no-auto-update --no-import --no-media --help -h ${global_flags}" -- "${cur}") )
            ;;
        update)
            COMPREPLY=( $(compgen -W "--repo --remote --branch --ref --no-media --help -h ${global_flags}" -- "${cur}") )
            ;;
        sync)
            COMPREPLY=( $(compgen -W "--source --workspace --channels --since --full --latest-only --concurrency --with-media --help -h ${global_flags}" -- "${cur}") )
            ;;
        import)
            COMPREPLY=( $(compgen -W "--workspace --dry-run --force --format --help -h ${global_flags}" -- "${cur}") )
            ;;
        purge)
            COMPREPLY=( $(compgen -W "--before --older-than --workspace --force --keep-media --vacuum --help -h ${global_flags}" -- "${cur}") )
            ;;
        tail)
            COMPREPLY=( $(compgen -W "--workspace --repair-every --help -h ${global_flags}" -- "${cur}") )
            ;;
        watch)
            COMPREPLY=( $(compgen -W "--desktop-every --help -h ${global_flags}" -- "${cur}") )
            ;;
        tui)
            COMPREPLY=( $(compgen -W "--json --limit --help -h ${global_flags}" -- "${cur}") )
            ;;
        search)
            COMPREPLY=( $(compgen -W "--workspace --help -h ${global_flags}" -- "${cur}") )
            ;;
        messages)
            COMPREPLY=( $(compgen -W "--workspace --channel --author --limit --help -h ${global_flags}" -- "${cur}") )
            ;;
        files)
            local files_subcommand=""
            for ((i=2; i < ${#words[@]}; i++)); do
                if [[ "${words[i]}" == "fetch" ]]; then
                    files_subcommand="fetch"
                    break
                fi
            done
            if [[ "${files_subcommand}" == "fetch" ]]; then
                COMPREPLY=( $(compgen -W "--workspace --channel --user --file --filename --type --since --before --missing --limit --all --force --max-bytes --help -h ${global_flags}" -- "${cur}") )
            else
                COMPREPLY=( $(compgen -W "fetch --workspace --channel --user --file --filename --type --since --before --missing --limit --all --help -h ${global_flags}" -- "${cur}") )
            fi
            ;;
        mentions)
            COMPREPLY=( $(compgen -W "--workspace --target --limit --help -h ${global_flags}" -- "${cur}") )
            ;;
        users)
            COMPREPLY=( $(compgen -W "--workspace --help -h ${global_flags}" -- "${cur}") )
            ;;
		channels)
			COMPREPLY=( $(compgen -W "--workspace --kind --help -h ${global_flags}" -- "${cur}") )
			;;
        completion)
            COMPREPLY=( $(compgen -W "bash zsh --help -h ${global_flags}" -- "${cur}") )
            ;;
        *)
            COMPREPLY=( $(compgen -W "${global_flags}" -- "${cur}") )
            ;;
    esac
}

complete -F _slacrawl slacrawl
`)
	return b.String()
}

func zshCompletionScript() string {
	commands := make([]string, 0, len(commandNames))
	for _, name := range commandNames {
		commands = append(commands, fmt.Sprintf(`"%s:%s command"`, name, name))
	}
	var b strings.Builder
	b.WriteString(`#compdef slacrawl

_slacrawl() {
  local -a commands
  commands=(
    `)
	b.WriteString(strings.Join(commands, "\n    "))
	b.WriteString(`
  )

  _arguments -C \
    '--config[override config path]:path:_files' \
    '--format[output format]:format:(text json log)' \
    '--json[compatibility alias for json output]' \
    '--no-color[disable ANSI color in text output]' \
    '1:command:->command' \
    '*::arg:->args'

  case $state in
    command)
      _describe 'command' commands
      ;;
    args)
      case $words[2] in
        init)
          _arguments '--workspace[workspace id]:workspace id:' '--db[database path]:database path:_files'
          ;;
        version)
          _arguments '--help[show help]'
          ;;
        check-update)
          _arguments '--json[json output]' '--help[show help]'
          ;;
        metadata)
          _arguments '--json[write crawlkit metadata JSON]' '--help[show help]'
          ;;
        report)
          _arguments '--help[show help]'
          ;;
        digest)
          _arguments '--since[lookback window]:duration:' '--workspace[workspace id]:workspace id:' '--channel[channel id or name]:channel:' '--top-n[top posters and mentions per channel]:count:'
          ;;
        analytics)
          if (( CURRENT == 3 )); then
            _values 'analytics subcommand' digest quiet trends
          else
            case $words[3] in
              digest)
                _arguments '--since[lookback window]:duration:' '--workspace[workspace id]:workspace id:' '--channel[channel id or name]:channel:' '--top-n[top posters and mentions per channel]:count:'
                ;;
              quiet)
                _arguments '--since[lookback window]:duration:' '--workspace[workspace id]:workspace id:' '--format[output format]:format:(text json log)' '--json[json output]'
                ;;
              trends)
                _arguments '--weeks[number of weeks]:count:' '--workspace[workspace id]:workspace id:' '--channel[channel id or name]:channel:' '--format[output format]:format:(text json log)' '--json[json output]'
                ;;
              *)
                _values 'analytics subcommand' digest quiet trends
                ;;
            esac
          fi
          ;;
        publish)
          _arguments '--repo[git repo path]:path:_files' '--remote[git remote]:remote:' '--branch[git branch]:branch:' '--message[commit message]:message:' '--tag[immutable snapshot tag]:tag:' '--no-commit[skip git commit]' '--push[push to origin]' '--no-media[omit cached media files]'
          ;;
        subscribe)
          _arguments '--repo[local clone path]:path:_files' '--db[database path]:path:_files' '--remote[git remote]:remote:' '--branch[git branch]:branch:' '--stale-after[auto-refresh age threshold]:duration:' '--no-auto-update[disable read-time auto refresh]' '--no-import[skip initial import]' '--no-media[skip restoring cached media]'
          ;;
        update)
          _arguments '--repo[local clone path]:path:_files' '--remote[git remote]:remote:' '--branch[git branch]:branch:' '--ref[historical Git ref to import]:ref:' '--no-media[skip restoring cached media]'
          ;;
        sync)
          _arguments '--source[sync source]:source:(api bot desktop wiretap mcp connector all)' '--workspace[workspace id]:workspace id:' '--channels[channel ids]:channels:' '--since[start timestamp]:timestamp:' '--full[run full sync]' '--latest-only[skip first-time historical backfills]' '--concurrency[worker count]:count:' '--with-media[fetch file media after sync]'
          ;;
        import)
          _arguments '--workspace[workspace id]:workspace id:' '--dry-run[walk and count without writing]' '--force[overwrite existing slack-export rows at the same rank]' '--format[output format]:format:(text json log)'
          ;;
        purge)
          _arguments '--before[absolute cutoff]:time:' '--older-than[relative cutoff]:duration:' '--workspace[workspace id]:workspace id:' '--force[execute deletion]' '--keep-media[retain unreferenced cached media]' '--vacuum[compact database after deletion]'
          ;;
        tail)
          _arguments '--workspace[workspace id]:workspace id:' '--repair-every[repair interval]:duration:'
          ;;
        watch)
          _arguments '--desktop-every[desktop refresh interval]:duration:'
          ;;
        tui)
          _arguments '--json[json output]' '--limit[row limit]:limit:'
          ;;
        search)
          _arguments '--workspace[workspace id]:workspace id:'
          ;;
        messages)
          _arguments '--workspace[workspace id]:workspace id:' '--channel[channel id]:channel id:' '--author[user id]:user id:' '--limit[row limit]:limit:'
          ;;
        files)
          if [[ $words[3] == fetch ]]; then
            _arguments '1:subcommand:(fetch)' '--workspace[workspace id]:workspace id:' '--channel[channel id]:channel id:' '--user[user id]:user id:' '--file[file id]:file id:' '--filename[filename filter]:text:' '--type[mimetype or filetype]:text:' '--since[RFC3339 lower bound]:time:' '--before[RFC3339 upper bound]:time:' '--missing[only missing cached media]' '--limit[row limit]:limit:' '--all[return all rows]' '--force[redownload cached files]' '--max-bytes[per-file cap]:bytes:'
          else
            _arguments '1:subcommand:(fetch)' '--workspace[workspace id]:workspace id:' '--channel[channel id]:channel id:' '--user[user id]:user id:' '--file[file id]:file id:' '--filename[filename filter]:text:' '--type[mimetype or filetype]:text:' '--since[RFC3339 lower bound]:time:' '--before[RFC3339 upper bound]:time:' '--missing[only missing cached media]' '--limit[row limit]:limit:' '--all[return all rows]'
          fi
          ;;
        mentions)
          _arguments '--workspace[workspace id]:workspace id:' '--target[target id or label]:target:' '--limit[row limit]:limit:'
          ;;
        users)
          _arguments '--workspace[workspace id]:workspace id:'
          ;;
		channels)
		  _arguments '--workspace[workspace id]:workspace id:' '--kind[channel kind]:kind:(im mpim public_channel private_channel)'
		  ;;
        completion)
          _values 'shell' bash zsh
          ;;
      esac
      ;;
  esac
}

_slacrawl "$@"
`)
	return b.String()
}
