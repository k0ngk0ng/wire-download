package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// completionCommand prints a shell completion script without loading the
// daemon configuration.  Keeping generation in the CLI means completion can
// be installed before the first `init` and works for both the bundled
// wirectl command and the standalone plugin binary.
func completionCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wirectl download completion bash|zsh|fish")
	}

	shell := strings.ToLower(strings.TrimSpace(args[0]))
	script, err := completionScript(shell)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(os.Stdout, script); err != nil {
		return fmt.Errorf("write %s completion: %w", shell, err)
	}
	return nil
}

func completionScript(shell string) (string, error) {
	var script string
	switch shell {
	case "bash":
		script = bashCompletionScript
	case "zsh":
		script = zshCompletionScript
	case "fish":
		script = fishCompletionScript
	default:
		return "", fmt.Errorf("unsupported shell %q (choose bash, zsh, or fish)", shell)
	}
	return script, nil
}

const bashCompletionScript = `# bash completion for wirectl download and wirectl-download.
# Install with: eval "$(wirectl download completion bash)"
_wirectl_download_file_complete() {
    local cur="$1" match
    COMPREPLY=()
    while IFS= read -r match; do
        COMPREPLY+=("$match")
    done < <(compgen -f -- "$cur")
}

_wirectl_download_dir_complete() {
    local cur="$1" match
    COMPREPLY=()
    while IFS= read -r match; do
        COMPREPLY+=("$match")
    done < <(compgen -d -- "$cur")
}

_wirectl_download_prefixed_file_complete() {
    local cur="$1" prefix value match
    prefix="${cur%%=*}="
    value="${cur#*=}"
    COMPREPLY=()
    while IFS= read -r match; do
        COMPREPLY+=("$prefix$match")
    done < <(compgen -f -- "$value")
}

_wirectl_download_prefixed_dir_complete() {
    local cur="$1" prefix value match
    prefix="${cur%%=*}="
    value="${cur#*=}"
    COMPREPLY=()
    while IFS= read -r match; do
        COMPREPLY+=("$prefix$match")
    done < <(compgen -d -- "$value")
}

_wirectl_download_values() {
    local cur="$1" values="$2"
    COMPREPLY=( $(compgen -W "$values" -- "$cur") )
}

_wirectl_download_completion() {
    local cur prev token command nested action program
    local offset=1 i
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    program="${COMP_WORDS[0]##*/}"

    # The root executable dispatches download to this plugin.  Do not make
    # the plugin's commands appear after another root command.
    if [[ "$program" == "wirectl" ]]; then
        if (( COMP_CWORD == 1 )); then
            _wirectl_download_values "$cur" "download version"
            return
        fi
        [[ "${COMP_WORDS[1]}" == "download" ]] || return
        offset=2
    fi

    # Find the command and (for search) its first nested command.  Option
    # values are skipped so a --data-dir value can never become a command.
    command=""
    nested=""
    action=""
    for ((i=offset; i<COMP_CWORD; i++)); do
        token="${COMP_WORDS[i]}"
        case "$token" in
            --data-dir|--downloads|--browser|--remote|--type|--source|--limit|--timeout|--ed2k-mode|--id|--name|--url|--api-key-env)
                ((i++))
                continue
                ;;
            --data-dir=*|--type=*|--source=*|--limit=*|--timeout=*|--ed2k-mode=*|--downloads=*|--browser=*|--remote=*|--id=*|--name=*|--url=*|--api-key-env=*)
                continue
                ;;
            -*)
                continue
                ;;
        esac
        if [[ -z "$command" ]]; then
            command="$token"
        elif [[ -z "$nested" ]]; then
            nested="$token"
        elif [[ "$command" == "search" && "$nested" == "sources" && -z "$action" ]]; then
            action="$token"
        fi
    done

    # Values for options that name a path are completed before option names.
    case "$prev" in
        --data-dir|--downloads)
            _wirectl_download_dir_complete "$cur"
            return
            ;;
        --browser)
            _wirectl_download_file_complete "$cur"
            return
            ;;
        --type)
            if [[ "$command" == "search" && "$nested" == "sources" && "$action" == "add" ]]; then
                _wirectl_download_values "$cur" "emule rss torznab nyaa animetosho dmhy btdig linuxtracker archive"
            else
                _wirectl_download_values "$cur" "all ed2k bt magnet torrent"
            fi
            return
            ;;
        --ed2k-mode)
            _wirectl_download_values "$cur" "all server global kad"
            return
            ;;
    esac
    case "$cur" in
        --data-dir=*|--downloads=*)
            _wirectl_download_prefixed_dir_complete "$cur"
            return
            ;;
        --browser=*)
            _wirectl_download_prefixed_file_complete "$cur"
            return
            ;;
        --type=*)
            if [[ "$command" == "search" && "$nested" == "sources" && "$action" == "add" ]]; then
                _wirectl_download_values "${cur#*=}" "emule rss torznab nyaa animetosho dmhy btdig linuxtracker archive"
            else
                _wirectl_download_values "${cur#*=}" "all ed2k bt magnet torrent"
            fi
            local value
            for i in "${!COMPREPLY[@]}"; do COMPREPLY[i]="--type=${COMPREPLY[i]}"; done
            return
            ;;
        --ed2k-mode=*)
            _wirectl_download_values "${cur#*=}" "all server global kad"
            for i in "${!COMPREPLY[@]}"; do COMPREPLY[i]="--ed2k-mode=${COMPREPLY[i]}"; done
            return
            ;;
    esac

    if [[ -z "$command" ]]; then
        if [[ "$cur" == -* ]]; then
            _wirectl_download_values "$cur" "--data-dir --help -h"
        else
            _wirectl_download_values "$cur" "search add login logout init version list watch pause resume remove daemon doctor bt emule servers completion"
        fi
        return
    fi

    case "$command" in
        search)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" "--type --source --limit --timeout --ed2k-mode --json --no-tui --help -h"
                else
                    _wirectl_download_values "$cur" "results cancel download sources"
                fi
                return
            fi
            case "$nested" in
                results)
                    [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--json --help -h"
                    return
                    ;;
                sources)
                    if [[ -z "$action" ]]; then
                        if [[ "$cur" == -* ]]; then
                            _wirectl_download_values "$cur" "--help -h"
                        else
                            _wirectl_download_values "$cur" "list add enable disable remove"
                        fi
                        return
                    fi
                    case "$action" in
                        list)
                            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--json --help -h"
                            ;;
                        add)
                            if [[ "$cur" == -* ]]; then
                                _wirectl_download_values "$cur" "--id --name --type --url --api-key-env --help -h"
                            fi
                            ;;
                        enable|disable|remove)
                            if [[ "$cur" != -* ]]; then
                                _wirectl_download_values "$cur" "ed2k nyaa animetosho dmhy btdig linuxtracker archive"
                            fi
                            ;;
                    esac
                    return
                    ;;
                cancel|download)
                    [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--help -h"
                    return
                    ;;
            esac
            return
            ;;
        add)
            if [[ "$cur" == -* ]]; then
                _wirectl_download_values "$cur" "--detach --help -h"
            else
                _wirectl_download_file_complete "$cur"
            fi
            return
            ;;
        init)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--downloads --help -h"
            return
            ;;
        login)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--browser --remote --import --help -h"
            return
            ;;
        list)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--json --help -h"
            return
            ;;
        daemon)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" "--help -h"
                else
                    _wirectl_download_values "$cur" "run start stop status"
                fi
            fi
            return
            ;;
        servers)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" "--help -h"
                else
                    _wirectl_download_values "$cur" "update"
                fi
            fi
            return
            ;;
        bt)
            if [[ -z "$nested" ]]; then
                [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--help -h" || _wirectl_download_values "$cur" "trackers"
            elif [[ "$nested" == "trackers" && "$cur" != -* ]]; then
                _wirectl_download_values "$cur" "list add remove"
            fi
            return
            ;;
        emule)
            if [[ -z "$nested" ]]; then
                [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--help -h" || _wirectl_download_values "$cur" "servers"
            elif [[ "$nested" == "servers" && "$cur" != -* ]]; then
                _wirectl_download_values "$cur" "update"
            fi
            return
            ;;
        completion)
            [[ "$cur" != -* ]] && _wirectl_download_values "$cur" "bash zsh fish"
            return
            ;;
        doctor|version|watch|logout|pause|resume|remove)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" "--help -h"
            return
            ;;
    esac
}

complete -o filenames -F _wirectl_download_completion wirectl
complete -o filenames -F _wirectl_download_completion wirectl-download
`

const zshCompletionScript = `#compdef wirectl wirectl-download
# zsh completion for wirectl download and wirectl-download.
_wirectl_download_files() {
    _files
}

_wirectl_download_dirs() {
    _files -/
}

_wirectl_download_prefixed_files() {
    local current="${words[CURRENT]}" prefix="${current%%=*}=" value="${current#*=}" saved
    saved="${words[CURRENT]}"
    words[CURRENT]="$value"
    _files -P "$prefix"
    words[CURRENT]="$saved"
}

_wirectl_download_prefixed_dirs() {
    local current="${words[CURRENT]}" prefix="${current%%=*}=" value="${current#*=}" saved
    saved="${words[CURRENT]}"
    words[CURRENT]="$value"
    _files -/ -P "$prefix"
    words[CURRENT]="$saved"
}

_wirectl_download_values() {
    local cur="$1"
    shift
    compadd -- "$@" 2>/dev/null
}

_wirectl_download_completion() {
    local program="${words[1]##*/}" cur prev token command nested action
    local offset=2 i
    local -a values
    cur="${words[CURRENT]}"
    prev="${words[CURRENT-1]}"

    if [[ "$program" == wirectl ]]; then
        if (( CURRENT == 2 )); then
            _wirectl_download_values "$cur" download version
            return
        fi
        [[ "${words[2]}" == download ]] || return
        offset=3
    fi

    command=""
    nested=""
    action=""
    for ((i=offset; i<CURRENT; i++)); do
        token="${words[i]}"
        case "$token" in
            --data-dir|--downloads|--browser|--remote|--type|--source|--limit|--timeout|--ed2k-mode|--id|--name|--url|--api-key-env)
                ((i+=1))
                continue
                ;;
            --data-dir=*|--type=*|--source=*|--limit=*|--timeout=*|--ed2k-mode=*|--downloads=*|--browser=*|--remote=*|--id=*|--name=*|--url=*|--api-key-env=*|-*)
                continue
                ;;
        esac
        if [[ -z "$command" ]]; then
            command="$token"
        elif [[ -z "$nested" ]]; then
            nested="$token"
        elif [[ "$command" == search && "$nested" == sources && -z "$action" ]]; then
            action="$token"
        fi
    done

    case "$prev" in
        --data-dir|--downloads)
            _wirectl_download_dirs
            return
            ;;
        --browser)
            _wirectl_download_files
            return
            ;;
        --type)
            if [[ "$command" == search && "$nested" == sources && "$action" == add ]]; then
                _wirectl_download_values "$cur" emule rss torznab nyaa animetosho dmhy btdig linuxtracker archive
            else
                _wirectl_download_values "$cur" all ed2k bt magnet torrent
            fi
            return
            ;;
        --ed2k-mode)
            _wirectl_download_values "$cur" all server global kad
            return
            ;;
    esac

    case "$cur" in
        --data-dir=*|--downloads=*)
            _wirectl_download_prefixed_dirs
            return
            ;;
        --browser=*)
            _wirectl_download_prefixed_files
            return
            ;;
        --type=*)
            if [[ "$command" == search && "$nested" == sources && "$action" == add ]]; then
                _wirectl_download_values "$cur" --type=emule --type=rss --type=torznab --type=nyaa --type=animetosho --type=dmhy --type=btdig --type=linuxtracker --type=archive
            else
                _wirectl_download_values "$cur" --type=all --type=ed2k --type=bt --type=magnet --type=torrent
            fi
            return
            ;;
        --ed2k-mode=*)
            _wirectl_download_values "$cur" --ed2k-mode=all --ed2k-mode=server --ed2k-mode=global --ed2k-mode=kad
            return
            ;;
    esac

    if [[ -z "$command" ]]; then
        if [[ "$cur" == -* ]]; then
            _wirectl_download_values "$cur" --data-dir --help -h
        else
            _wirectl_download_values "$cur" search add login logout init version list watch pause resume remove daemon doctor bt emule servers completion
        fi
        return
    fi

    case "$command" in
        search)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" --type --source --limit --timeout --ed2k-mode --json --no-tui --help -h
                else
                    _wirectl_download_values "$cur" results cancel download sources
                fi
                return
            fi
            case "$nested" in
                results)
                    [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --json --help -h
                    ;;
                sources)
                    if [[ -z "$action" ]]; then
                        if [[ "$cur" == -* ]]; then
                            _wirectl_download_values "$cur" --help -h
                        else
                            _wirectl_download_values "$cur" list add enable disable remove
                        fi
                    else
                        case "$action" in
                            list)
                                [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --json --help -h
                                ;;
                            add)
                                [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --id --name --type --url --api-key-env --help -h
                                ;;
                            enable|disable|remove)
                                [[ "$cur" != -* ]] && _wirectl_download_values "$cur" ed2k nyaa animetosho dmhy btdig linuxtracker archive
                                ;;
                        esac
                    fi
                    ;;
                cancel|download)
                    [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --help -h
                    ;;
            esac
            return
            ;;
        add)
            if [[ "$cur" == -* ]]; then
                _wirectl_download_values "$cur" --detach --help -h
            else
                _wirectl_download_files
            fi
            return
            ;;
        init)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --downloads --help -h
            return
            ;;
        login)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --browser --remote --import --help -h
            return
            ;;
        list)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --json --help -h
            return
            ;;
        daemon)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" --help -h
                else
                    _wirectl_download_values "$cur" run start stop status
                fi
            fi
            return
            ;;
        servers)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" --help -h
                else
                    _wirectl_download_values "$cur" update
                fi
            fi
            return
            ;;
        bt)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" --help -h
                else
                    _wirectl_download_values "$cur" trackers
                fi
            elif [[ "$nested" == trackers && "$cur" != -* ]]; then
                _wirectl_download_values "$cur" list add remove
            fi
            return
            ;;
        emule)
            if [[ -z "$nested" ]]; then
                if [[ "$cur" == -* ]]; then
                    _wirectl_download_values "$cur" --help -h
                else
                    _wirectl_download_values "$cur" servers
                fi
            elif [[ "$nested" == servers && "$cur" != -* ]]; then
                _wirectl_download_values "$cur" update
            fi
            return
            ;;
        completion)
            [[ "$cur" != -* ]] && _wirectl_download_values "$cur" bash zsh fish
            return
            ;;
        doctor|version|watch|logout|pause|resume|remove)
            [[ "$cur" == -* ]] && _wirectl_download_values "$cur" --help -h
            return
            ;;
    esac
}

compdef _wirectl_download_completion wirectl
compdef _wirectl_download_completion wirectl-download
`

const fishCompletionScript = `# fish completion for wirectl download and wirectl-download.
function __wirectl_download_path
    __fish_complete_path
end

function __wirectl_download_values
    set -l current (commandline -ct)
    for value in $argv
        string match -q -- "$current*" -- $value; and echo $value
    end
end

function __wirectl_download_complete
    set -l words (commandline -opc)
    set -l current (commandline -ct)
    set -l previous
    if test (count $words) -gt 0
        set previous $words[-1]
    end

    set -l program (string replace -r '.*/' '' -- $words[1])
    set -l offset 2
    if test "$program" = wirectl
        if test (count $words) -eq 1
            __wirectl_download_values download version
            return
        end
        test "$words[2]" = download; or return
        set offset 3
    end

    set -l command
    set -l nested
    set -l action
    set -l skip 0
    for token in $words[$offset..-1]
        if test $skip -eq 1
            set skip 0
            continue
        end
        switch $token
            case --data-dir --downloads --browser --remote --type --source --limit --timeout --ed2k-mode --id --name --url --api-key-env
                set skip 1
                continue
            case '--data-dir=*' '--type=*' '--source=*' '--limit=*' '--timeout=*' '--ed2k-mode=*' '--downloads=*' '--browser=*' '--remote=*' '--id=*' '--name=*' '--url=*' '--api-key-env=*' '-*'
                continue
        end
        if test -z "$command"
            set command $token
        else if test -z "$nested"
            set nested $token
        else if test "$command" = search; and test "$nested" = sources; and test -z "$action"
            set action $token
        end
    end

    switch $previous
        case --data-dir --downloads --browser
            __wirectl_download_path
            return
        case --type
            if test "$command" = search; and test "$nested" = sources; and test "$action" = add
                __wirectl_download_values emule rss torznab nyaa animetosho dmhy btdig linuxtracker archive
            else
                __wirectl_download_values all ed2k bt magnet torrent
            end
            return
        case --ed2k-mode
            __wirectl_download_values all server global kad
            return
    end

    if test -z "$command"
        if string match -q -- '-*' "$current"
            __wirectl_download_values --data-dir --help -h
        else
            __wirectl_download_values search add login logout init version list watch pause resume remove daemon doctor bt emule servers completion
        end
        return
    end

    switch $command
        case search
            if test -z "$nested"
                if string match -q -- '-*' "$current"
                    __wirectl_download_values --type --source --limit --timeout --ed2k-mode --json --no-tui --help -h
                else
                    __wirectl_download_values results cancel download sources
                end
                return
            end
            switch $nested
                case results
                    string match -q -- '-*' "$current"; and __wirectl_download_values --json --help -h
                case sources
                    if test -z "$action"
                        if string match -q -- '-*' "$current"
                            __wirectl_download_values --help -h
                        else
                            __wirectl_download_values list add enable disable remove
                        end
                    else
                        switch $action
                            case list
                                string match -q -- '-*' "$current"; and __wirectl_download_values --json --help -h
                            case add
                                string match -q -- '-*' "$current"; and __wirectl_download_values --id --name --type --url --api-key-env --help -h
                            case enable disable remove
                                not string match -q -- '-*' "$current"; and __wirectl_download_values ed2k nyaa animetosho dmhy btdig linuxtracker archive
                        end
                    end
                case cancel download
                    string match -q -- '-*' "$current"; and __wirectl_download_values --help -h
            end
            return
        case add
            if string match -q -- '-*' "$current"
                __wirectl_download_values --detach --help -h
            else
                __wirectl_download_path
            end
            return
        case init
            string match -q -- '-*' "$current"; and __wirectl_download_values --downloads --help -h
            return
        case login
            string match -q -- '-*' "$current"; and __wirectl_download_values --browser --remote --import --help -h
            return
        case list
            string match -q -- '-*' "$current"; and __wirectl_download_values --json --help -h
            return
        case daemon
            if test -z "$nested"
                if string match -q -- '-*' "$current"
                    __wirectl_download_values --help -h
                else
                    __wirectl_download_values run start stop status
                end
            end
            return
        case servers
            if test -z "$nested"
                if string match -q -- '-*' "$current"
                    __wirectl_download_values --help -h
                else
                    __wirectl_download_values update
                end
            end
            return
        case bt
            if test -z "$nested"
                if string match -q -- '-*' "$current"
                    __wirectl_download_values --help -h
                else
                    __wirectl_download_values trackers
                end
            else if test "$nested" = trackers; and not string match -q -- '-*' "$current"
                __wirectl_download_values list add remove
            end
            return
        case emule
            if test -z "$nested"
                if string match -q -- '-*' "$current"
                    __wirectl_download_values --help -h
                else
                    __wirectl_download_values servers
                end
            else if test "$nested" = servers; and not string match -q -- '-*' "$current"
                __wirectl_download_values update
            end
            return
        case completion
            not string match -q -- '-*' "$current"; and __wirectl_download_values bash zsh fish
            return
        case doctor version watch logout pause resume remove
            string match -q -- '-*' "$current"; and __wirectl_download_values --help -h
            return
    end
end

complete -c wirectl-download -f -a '(__wirectl_download_complete)'
complete -c wirectl -f -a '(__wirectl_download_complete)'
`
