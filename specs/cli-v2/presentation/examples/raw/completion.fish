# fish completion for cq; generated from specs/cli-v2/commands.json
complete -c cq -e
function __cq_command_words
    set -l tokens (commandline -opc)
    set -e tokens[1]
    set -l result
    set -l options_open 1
    for token in $tokens
        if test $options_open -eq 1; and test "$token" = --
            set options_open 0
            continue
        end
        if test $options_open -eq 1
            switch $token
                case -h -j -v --help --json --version '-h=*' '-j=*' '-v=*' '--help=*' '--json=*' '--version=*'
                    continue
            end
        end
        set -a result $token
    end
    printf '%s\n' $result
end
function __cq_path_is
    set -l tokens (__cq_command_words)
    set -l actual ""
    set -l expected ""
    if test (count $tokens) -gt 0
        set actual (string join " " -- $tokens)
    end
    if test (count $argv) -gt 0
        set expected (string join " " -- $argv)
    end
    test "$actual" = "$expected"
end
function __cq_has_path
    set -l tokens (__cq_command_words)
    test (count $tokens) -ge (count $argv); or return 1
    for index in (seq (count $argv))
        test "$tokens[$index]" = "$argv[$index]"; or return 1
    end
end
function __cq_options_open
    not contains -- -- (commandline -opc)
end
complete -c cq -f -n '__cq_path_is; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_path_is; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_path_is; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n __cq_path_is -a auth
complete -c cq -f -n __cq_path_is -a check
complete -c cq -f -n __cq_path_is -a claude
complete -c cq -f -n __cq_path_is -a codex
complete -c cq -f -n __cq_path_is -a completion
complete -c cq -f -n __cq_path_is -a gemini
complete -c cq -f -n __cq_path_is -a help
complete -c cq -f -n __cq_path_is -a models
complete -c cq -f -n __cq_path_is -a proxy
complete -c cq -f -n __cq_path_is -a service
complete -c cq -f -n __cq_path_is -a version
complete -c cq -f -n '__cq_path_is auth' -a refresh
complete -c cq -f -n '__cq_path_is claude' -a account
complete -c cq -f -n '__cq_path_is claude' -a proxy
complete -c cq -f -n '__cq_path_is claude account' -a activate
complete -c cq -f -n '__cq_path_is claude account' -a list
complete -c cq -f -n '__cq_path_is claude account' -a login
complete -c cq -f -n '__cq_path_is claude account' -a remove
complete -c cq -f -n '__cq_path_is claude proxy' -a pin
complete -c cq -f -n '__cq_path_is claude proxy pin' -a clear
complete -c cq -f -n '__cq_path_is claude proxy pin' -a set
complete -c cq -f -n '__cq_path_is claude proxy pin' -a show
complete -c cq -f -n '__cq_path_is codex' -a account
complete -c cq -f -n '__cq_path_is codex' -a proxy
complete -c cq -f -n '__cq_path_is codex' -a reset
complete -c cq -f -n '__cq_path_is codex account' -a activate
complete -c cq -f -n '__cq_path_is codex account' -a list
complete -c cq -f -n '__cq_path_is codex account' -a login
complete -c cq -f -n '__cq_path_is codex account' -a remove
complete -c cq -f -n '__cq_path_is codex proxy' -a canary
complete -c cq -f -n '__cq_path_is codex proxy' -a credential-endpoint
complete -c cq -f -n '__cq_path_is codex proxy' -a fallback
complete -c cq -f -n '__cq_path_is codex proxy' -a fixture
complete -c cq -f -n '__cq_path_is codex proxy' -a hook
complete -c cq -f -n '__cq_path_is codex proxy' -a lease
complete -c cq -f -n '__cq_path_is codex proxy' -a pin
complete -c cq -f -n '__cq_path_is codex proxy' -a policy
complete -c cq -f -n '__cq_path_is codex proxy' -a pool
complete -c cq -f -n '__cq_path_is codex proxy' -a prime
complete -c cq -f -n '__cq_path_is codex proxy' -a readiness
complete -c cq -f -n '__cq_path_is codex proxy' -a reserve
complete -c cq -f -n '__cq_path_is codex proxy' -a session
complete -c cq -f -n '__cq_path_is codex proxy' -a trace
complete -c cq -f -n '__cq_path_is codex proxy' -a validate
complete -c cq -f -n '__cq_path_is codex proxy canary' -a start
complete -c cq -f -n '__cq_path_is codex proxy canary' -a status
complete -c cq -f -n '__cq_path_is codex proxy canary' -a stop
complete -c cq -f -n '__cq_path_is codex proxy credential-endpoint' -a legacy
complete -c cq -f -n '__cq_path_is codex proxy credential-endpoint legacy' -a activate
complete -c cq -f -n '__cq_path_is codex proxy credential-endpoint legacy' -a finalise
complete -c cq -f -n '__cq_path_is codex proxy credential-endpoint legacy' -a inspect
complete -c cq -f -n '__cq_path_is codex proxy credential-endpoint legacy' -a prepare
complete -c cq -f -n '__cq_path_is codex proxy credential-endpoint legacy' -a resume
complete -c cq -f -n '__cq_path_is codex proxy credential-endpoint legacy' -a rollback
complete -c cq -f -n '__cq_path_is codex proxy fallback' -a clear
complete -c cq -f -n '__cq_path_is codex proxy fallback' -a set
complete -c cq -f -n '__cq_path_is codex proxy fallback' -a show
complete -c cq -f -n '__cq_path_is codex proxy fixture' -a create
complete -c cq -f -n '__cq_path_is codex proxy hook' -a stop
complete -c cq -f -n '__cq_path_is codex proxy lease' -a invalidate
complete -c cq -f -n '__cq_path_is codex proxy pin' -a clear
complete -c cq -f -n '__cq_path_is codex proxy pin' -a set
complete -c cq -f -n '__cq_path_is codex proxy pin' -a show
complete -c cq -f -n '__cq_path_is codex proxy policy' -a apply
complete -c cq -f -n '__cq_path_is codex proxy policy' -a show
complete -c cq -f -n '__cq_path_is codex proxy pool' -a rename
complete -c cq -f -n '__cq_path_is codex proxy pool' -a set
complete -c cq -f -n '__cq_path_is codex proxy pool' -a value
complete -c cq -f -n '__cq_path_is codex proxy prime' -a disable
complete -c cq -f -n '__cq_path_is codex proxy prime' -a enable
complete -c cq -f -n '__cq_path_is codex proxy prime' -a status
complete -c cq -f -n '__cq_path_is codex proxy readiness' -a show
complete -c cq -f -n '__cq_path_is codex proxy reserve' -a clear
complete -c cq -f -n '__cq_path_is codex proxy reserve' -a disable
complete -c cq -f -n '__cq_path_is codex proxy reserve' -a enable
complete -c cq -f -n '__cq_path_is codex proxy reserve' -a set
complete -c cq -f -n '__cq_path_is codex proxy reserve' -a status
complete -c cq -f -n '__cq_path_is codex proxy reserve' -a windows
complete -c cq -f -n '__cq_path_is codex proxy session' -a bind
complete -c cq -f -n '__cq_path_is codex proxy session' -a digest
complete -c cq -f -n '__cq_path_is codex proxy session' -a list
complete -c cq -f -n '__cq_path_is codex proxy session' -a show
complete -c cq -f -n '__cq_path_is codex proxy session' -a unbind
complete -c cq -f -n '__cq_path_is codex proxy validate' -a http
complete -c cq -f -n '__cq_path_is codex proxy validate' -a websocket
complete -c cq -f -n '__cq_path_is codex reset' -a list
complete -c cq -f -n '__cq_path_is codex reset' -a recommend
complete -c cq -f -n '__cq_path_is codex reset' -a use
complete -c cq -f -n '__cq_path_is gemini' -a account
complete -c cq -f -n '__cq_path_is gemini account' -a show
complete -c cq -f -n '__cq_path_is models' -a list
complete -c cq -f -n '__cq_path_is models' -a overlay
complete -c cq -f -n '__cq_path_is models' -a refresh
complete -c cq -f -n '__cq_path_is models overlay' -a add
complete -c cq -f -n '__cq_path_is models overlay' -a prune
complete -c cq -f -n '__cq_path_is models overlay' -a remove
complete -c cq -f -n '__cq_path_is proxy' -a candidate
complete -c cq -f -n '__cq_path_is proxy' -a health
complete -c cq -f -n '__cq_path_is proxy' -a operation
complete -c cq -f -n '__cq_path_is proxy' -a rescue
complete -c cq -f -n '__cq_path_is proxy' -a serve
complete -c cq -f -n '__cq_path_is proxy' -a state
complete -c cq -f -n '__cq_path_is proxy' -a status
complete -c cq -f -n '__cq_path_is proxy candidate' -a client-safety
complete -c cq -f -n '__cq_path_is proxy candidate' -a prepare
complete -c cq -f -n '__cq_path_is proxy candidate' -a receipt
complete -c cq -f -n '__cq_path_is proxy candidate' -a release
complete -c cq -f -n '__cq_path_is proxy candidate' -a remove
complete -c cq -f -n '__cq_path_is proxy candidate' -a start
complete -c cq -f -n '__cq_path_is proxy candidate' -a status
complete -c cq -f -n '__cq_path_is proxy candidate' -a stop
complete -c cq -f -n '__cq_path_is proxy candidate client-safety' -a refresh
complete -c cq -f -n '__cq_path_is proxy candidate receipt' -a show
complete -c cq -f -n '__cq_path_is proxy candidate release' -a activate
complete -c cq -f -n '__cq_path_is proxy candidate release' -a validate
complete -c cq -f -n '__cq_path_is proxy operation' -a status
complete -c cq -f -n '__cq_path_is proxy rescue' -a enter
complete -c cq -f -n '__cq_path_is proxy rescue' -a exit
complete -c cq -f -n '__cq_path_is proxy rescue' -a status
complete -c cq -f -n '__cq_path_is proxy state' -a initialise
complete -c cq -f -n '__cq_path_is service' -a install
complete -c cq -f -n '__cq_path_is service' -a restart
complete -c cq -f -n '__cq_path_is service' -a start
complete -c cq -f -n '__cq_path_is service' -a status
complete -c cq -f -n '__cq_path_is service' -a stop
complete -c cq -f -n '__cq_path_is service' -a uninstall
complete -c cq -f -n '__cq_has_path auth; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path auth; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path auth; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path auth refresh; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path auth refresh; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path auth refresh; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path auth refresh' -a 'claude codex'
complete -c cq -f -n '__cq_has_path check; and __cq_options_open; and not __fish_seen_argument -l fresh' -l fresh
complete -c cq -f -n '__cq_has_path check; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path check; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path check; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path check; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path check' -a 'claude codex gemini'
complete -c cq -f -n '__cq_has_path claude; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude account; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude account; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude account; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude account activate; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path claude account activate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude account activate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude account activate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude account list; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path claude account list; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude account list; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude account list; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude account login; and __cq_options_open; and not __fish_seen_argument -l activate' -l activate
complete -c cq -f -n '__cq_has_path claude account login; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path claude account login; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude account login; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude account login; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude account remove; and __cq_options_open; and not __fish_seen_argument -l yes' -l yes
complete -c cq -f -n '__cq_has_path claude account remove; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path claude account remove; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude account remove; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude account remove; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude proxy; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude proxy; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude proxy; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude proxy pin; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude proxy pin; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude proxy pin; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path claude proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude proxy pin set; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path claude proxy pin set; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude proxy pin set; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude proxy pin set; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path claude proxy pin show; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path claude proxy pin show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path claude proxy pin show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path claude proxy pin show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex account; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex account; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex account; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex account activate; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex account activate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex account activate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex account activate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex account list; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex account list; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex account list; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex account list; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex account login; and __cq_options_open; and not __fish_seen_argument -l activate' -l activate
complete -c cq -f -n '__cq_has_path codex account login; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex account login; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex account login; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex account login; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex account remove; and __cq_options_open; and not __fish_seen_argument -l yes' -l yes
complete -c cq -f -n '__cq_has_path codex account remove; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex account remove; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex account remove; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex account remove; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy canary; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy canary; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy canary; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy canary start; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy canary start; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy canary start; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy canary status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy canary status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy canary status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy canary stop; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy canary stop; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy canary stop; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy credential-endpoint legacy activate; and __cq_options_open; and not __fish_seen_argument -l ticket-file' -l ticket-file -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy activate; and __cq_options_open; and not __fish_seen_argument -l confirm-stopped-and-drained' -l confirm-stopped-and-drained
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy activate; and __cq_options_open; and not __fish_seen_argument -l non-interactive' -l non-interactive
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy activate; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy activate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy activate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy activate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy credential-endpoint legacy finalise; and __cq_options_open; and not __fish_seen_argument -l ticket-file' -l ticket-file -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy finalise; and __cq_options_open; and not __fish_seen_argument -l confirm-candidate-healthy' -l confirm-candidate-healthy
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy finalise; and __cq_options_open; and not __fish_seen_argument -l non-interactive' -l non-interactive
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy finalise; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy finalise; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy finalise; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy finalise; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy inspect; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy inspect; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy inspect; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy inspect; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy credential-endpoint legacy prepare; and __cq_options_open; and not __fish_seen_argument -l snapshot-file' -l snapshot-file -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy prepare; and __cq_options_open; and not __fish_seen_argument -l confirm-stopped-and-drained' -l confirm-stopped-and-drained
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy prepare; and __cq_options_open; and not __fish_seen_argument -l non-interactive' -l non-interactive
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy prepare; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy prepare; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy prepare; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy prepare; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy credential-endpoint legacy resume; and __cq_options_open; and not __fish_seen_argument -l ticket-file' -l ticket-file -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy resume; and __cq_options_open; and not __fish_seen_argument -l confirm-stopped-and-drained' -l confirm-stopped-and-drained
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy resume; and __cq_options_open; and not __fish_seen_argument -l non-interactive' -l non-interactive
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy resume; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy resume; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy resume; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy resume; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy credential-endpoint legacy rollback; and __cq_options_open; and not __fish_seen_argument -l ticket-file' -l ticket-file -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy rollback; and __cq_options_open; and not __fish_seen_argument -l confirm-stopped-and-drained' -l confirm-stopped-and-drained
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy rollback; and __cq_options_open; and not __fish_seen_argument -l non-interactive' -l non-interactive
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy rollback; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy rollback; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy rollback; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy credential-endpoint legacy rollback; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy fallback; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy fallback; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy fallback; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy fallback clear; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy fallback clear; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy fallback clear; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy fallback clear; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy fallback set; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy fallback set; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy fallback set; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy fallback set; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy fallback show; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy fallback show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy fallback show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy fallback show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy fixture; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy fixture; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy fixture; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy fixture create; and __cq_options_open; and not __fish_seen_argument -l input' -l input -r
complete -c cq -n '__cq_has_path codex proxy fixture create; and __cq_options_open; and not __fish_seen_argument -l output' -l output -r
complete -c cq -f -n '__cq_has_path codex proxy fixture create; and __cq_options_open; and not __fish_seen_argument -l content-encoding' -l content-encoding -r -a 'auto identity zstd'
complete -c cq -f -n '__cq_has_path codex proxy fixture create; and __cq_options_open; and not __fish_seen_argument -l metadata-json' -l metadata-json -r
complete -c cq -f -n '__cq_has_path codex proxy fixture create; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy fixture create; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy fixture create; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy hook; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy hook; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy hook; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy hook stop; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy hook stop; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy hook stop; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy lease; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy lease; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy lease; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy lease invalidate; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy lease invalidate; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy lease invalidate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy lease invalidate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy lease invalidate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pin; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pin; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pin; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pin clear; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pin set; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy pin set; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pin set; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pin set; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pin show; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy pin show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pin show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pin show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy policy; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy policy; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy policy; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy policy apply; and __cq_options_open; and not __fish_seen_argument -l file' -l file -r
complete -c cq -n '__cq_has_path codex proxy policy apply; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path codex proxy policy apply; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy policy apply; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy policy apply; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy policy apply; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy policy apply; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path codex proxy policy show; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path codex proxy policy show; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy policy show; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy policy show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy policy show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy policy show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pool; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pool; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pool; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pool rename; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy pool rename; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy pool rename; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pool rename; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pool rename; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pool set; and __cq_options_open' -l account -r
complete -c cq -f -n '__cq_has_path codex proxy pool set; and __cq_options_open; and not __fish_seen_argument -l value' -l value -r
complete -c cq -f -n '__cq_has_path codex proxy pool set; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy pool set; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy pool set; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pool set; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pool set; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy pool value; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy pool value; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy pool value; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy pool value; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy pool value; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy prime; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy prime; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy prime; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy prime disable; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy prime disable; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy prime disable; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy prime disable; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy prime enable; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy prime enable; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy prime enable; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy prime enable; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy prime status; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy prime status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy prime status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy prime status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy readiness; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy readiness; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy readiness; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy readiness show; and __cq_options_open; and not __fish_seen_argument -l client-build' -l client-build -r
complete -c cq -n '__cq_has_path codex proxy readiness show; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path codex proxy readiness show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy readiness show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy readiness show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy reserve; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy reserve; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy reserve; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy reserve clear; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy reserve clear; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy reserve clear; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy reserve clear; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy reserve clear; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy reserve disable; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy reserve disable; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy reserve disable; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy reserve disable; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy reserve disable; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy reserve enable; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy reserve enable; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy reserve enable; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy reserve enable; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy reserve enable; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy reserve set; and __cq_options_open; and not __fish_seen_argument -l window' -l window -r
complete -c cq -f -n '__cq_has_path codex proxy reserve set; and __cq_options_open; and not __fish_seen_argument -l percent' -l percent -r
complete -c cq -f -n '__cq_has_path codex proxy reserve set; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy reserve set; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy reserve set; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy reserve set; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy reserve set; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy reserve status; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy reserve status; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy reserve status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy reserve status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy reserve status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy reserve windows; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy reserve windows; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy reserve windows; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy reserve windows; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy reserve windows; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy session; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy session; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy session; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l pool' -l pool -r
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l session-id' -l session-id -r
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l session-id-stdin' -l session-id-stdin
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l digest' -l digest -r
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy session bind; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l session-id' -l session-id -r
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l session-id-stdin' -l session-id-stdin
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l digest' -l digest -r
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy session digest; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy session list; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy session list; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy session list; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy session list; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy session list; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l session-id' -l session-id -r
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l session-id-stdin' -l session-id-stdin
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l digest' -l digest -r
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy session show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l session-id' -l session-id -r
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l session-id-stdin' -l session-id-stdin
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l digest' -l digest -r
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy session unbind; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l session' -l session -r
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l trace' -l trace -r
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l since' -l since -r
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l tail' -l tail -r
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l follow' -l follow
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l payload' -l payload
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy trace; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy validate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy validate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy validate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy validate http; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path codex proxy validate http; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy validate http; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy validate http; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy validate http; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex proxy validate websocket; and __cq_options_open; and not __fish_seen_argument -l client-build' -l client-build -r
complete -c cq -n '__cq_has_path codex proxy validate websocket; and __cq_options_open; and not __fish_seen_argument -l client-executable' -l client-executable -r
complete -c cq -n '__cq_has_path codex proxy validate websocket; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path codex proxy validate websocket; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex proxy validate websocket; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex proxy validate websocket; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex proxy validate websocket; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex reset; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex reset; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex reset; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex reset list; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex reset list; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex reset list; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex reset list; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex reset recommend; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex reset recommend; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex reset recommend; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex reset recommend; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path codex reset use; and __cq_options_open; and not __fish_seen_argument -l credit' -l credit -r
complete -c cq -f -n '__cq_has_path codex reset use; and __cq_options_open; and not __fish_seen_argument -l yes' -l yes
complete -c cq -f -n '__cq_has_path codex reset use; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path codex reset use; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path codex reset use; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path codex reset use; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path completion; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path completion; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path completion; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path completion' -a 'bash zsh fish'
complete -c cq -f -n '__cq_has_path gemini; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path gemini; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path gemini; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path gemini account; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path gemini account; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path gemini account; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path gemini account show; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path gemini account show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path gemini account show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path gemini account show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path help; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path help; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path help; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path models; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path models; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path models; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path models list; and __cq_options_open; and not __fish_seen_argument -l provider' -l provider -r -a 'claude codex'
complete -c cq -f -n '__cq_has_path models list; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path models list; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path models list; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path models overlay; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path models overlay; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path models overlay; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path models overlay add; and __cq_options_open; and not __fish_seen_argument -l provider' -l provider -r -a 'claude codex'
complete -c cq -f -n '__cq_has_path models overlay add; and __cq_options_open; and not __fish_seen_argument -l id' -l id -r
complete -c cq -f -n '__cq_has_path models overlay add; and __cq_options_open; and not __fish_seen_argument -l clone-from' -l clone-from -r
complete -c cq -f -n '__cq_has_path models overlay add; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path models overlay add; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path models overlay add; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path models overlay prune; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path models overlay prune; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path models overlay prune; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path models overlay remove; and __cq_options_open; and not __fish_seen_argument -l provider' -l provider -r -a 'claude codex'
complete -c cq -f -n '__cq_has_path models overlay remove; and __cq_options_open; and not __fish_seen_argument -l id' -l id -r
complete -c cq -f -n '__cq_has_path models overlay remove; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path models overlay remove; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path models overlay remove; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path models refresh; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path models refresh; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path models refresh; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy candidate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy candidate client-safety; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate client-safety; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate client-safety; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate client-safety refresh; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate client-safety refresh; and __cq_options_open; and not __fish_seen_argument -l validation-run-id' -l validation-run-id -r
complete -c cq -f -n '__cq_has_path proxy candidate client-safety refresh; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy candidate client-safety refresh; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate client-safety refresh; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate client-safety refresh; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l source-config' -l source-config -r
complete -c cq -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l target-release-bundle' -l target-release-bundle -r
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l release-digest' -l release-digest -r
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l client-build' -l client-build -r
complete -c cq -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l client-executable' -l client-executable -r
complete -c cq -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l client-registry' -l client-registry -r
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l credential-mode' -l credential-mode -r -a 'none read-only'
complete -c cq -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l credential-manifest' -l credential-manifest -r
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l confirm-read-only-credentials' -l confirm-read-only-credentials
complete -c cq -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l policy-snapshot' -l policy-snapshot -r
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l confirm-payload-capture' -l confirm-payload-capture
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate prepare; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy candidate receipt; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate receipt; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate receipt; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate receipt show; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate receipt show; and __cq_options_open; and not __fish_seen_argument -l attempt-id' -l attempt-id -r
complete -c cq -f -n '__cq_has_path proxy candidate receipt show; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy candidate receipt show; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate receipt show; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate receipt show; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy candidate release; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate release; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate release; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l release-digest' -l release-digest -r
complete -c cq -f -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l validation-run-id' -l validation-run-id -r
complete -c cq -f -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l confirm-artifact-switch' -l confirm-artifact-switch
complete -c cq -f -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate release activate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l target-release-bundle' -l target-release-bundle -r
complete -c cq -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l rollback-bundle' -l rollback-bundle -r
complete -c cq -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l rollback-receipt' -l rollback-receipt -r
complete -c cq -f -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l rollback-receipt-digest' -l rollback-receipt-digest -r
complete -c cq -f -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l client-build' -l client-build -r
complete -c cq -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l client-executable' -l client-executable -r
complete -c cq -f -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l validation-run-id' -l validation-run-id -r
complete -c cq -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l receipt-file' -l receipt-file -r
complete -c cq -f -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l confirm-control-health' -l confirm-control-health
complete -c cq -f -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate release validate; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate remove; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate remove; and __cq_options_open; and not __fish_seen_argument -l confirm-candidate-state-loss' -l confirm-candidate-state-loss
complete -c cq -f -n '__cq_has_path proxy candidate remove; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate remove; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate remove; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate start; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate start; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy candidate start; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate start; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate start; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate status; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate status; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy candidate status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy candidate stop; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy candidate stop; and __cq_options_open; and not __fish_seen_argument -l confirm-client-stopped' -l confirm-client-stopped
complete -c cq -f -n '__cq_has_path proxy candidate stop; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy candidate stop; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy candidate stop; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy health; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path proxy health; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy health; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy health; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy health; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy operation; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy operation; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy operation; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy operation status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy operation status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy operation status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy rescue; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy rescue; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy rescue; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy rescue enter; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path proxy rescue enter; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy rescue enter; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy rescue enter; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy rescue enter; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy rescue exit; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path proxy rescue exit; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy rescue exit; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy rescue exit; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy rescue exit; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy rescue status; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path proxy rescue status; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy rescue status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy rescue status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy rescue status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy serve; and __cq_options_open; and not __fish_seen_argument -l port' -l port -r
complete -c cq -f -n '__cq_has_path proxy serve; and __cq_options_open; and not __fish_seen_argument -l migrate-legacy-managed' -l migrate-legacy-managed
complete -c cq -f -n '__cq_has_path proxy serve; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy serve; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy serve; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path proxy state; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy state; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy state; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy state initialise; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy state initialise; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy state initialise; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy state initialise; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy state initialise; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -n '__cq_has_path proxy status; and __cq_options_open; and not __fish_seen_argument -l state-dir' -l state-dir -r
complete -c cq -f -n '__cq_has_path proxy status; and __cq_options_open; and not __fish_seen_argument -l strict' -l strict
complete -c cq -f -n '__cq_has_path proxy status; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path proxy status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path proxy status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path proxy status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path service; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path service; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path service; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path service install; and __cq_options_open; and not __fish_seen_argument -l component' -l component -r -a 'all proxy token-refresh'
complete -c cq -f -n '__cq_has_path service install; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path service install; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path service install; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path service install; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path service restart; and __cq_options_open; and not __fish_seen_argument -l component' -l component -r -a 'all proxy token-refresh'
complete -c cq -f -n '__cq_has_path service restart; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path service restart; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path service restart; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path service restart; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path service start; and __cq_options_open; and not __fish_seen_argument -l component' -l component -r -a 'all proxy token-refresh'
complete -c cq -f -n '__cq_has_path service start; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path service start; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path service start; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path service start; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path service status; and __cq_options_open; and not __fish_seen_argument -l component' -l component -r -a 'all proxy token-refresh'
complete -c cq -f -n '__cq_has_path service status; and __cq_options_open; and not __fish_seen_argument -l strict' -l strict
complete -c cq -f -n '__cq_has_path service status; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path service status; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path service status; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path service status; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path service stop; and __cq_options_open; and not __fish_seen_argument -l component' -l component -r -a 'all proxy token-refresh'
complete -c cq -f -n '__cq_has_path service stop; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path service stop; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path service stop; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path service stop; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path service uninstall; and __cq_options_open; and not __fish_seen_argument -l component' -l component -r -a 'all proxy token-refresh'
complete -c cq -f -n '__cq_has_path service uninstall; and __cq_options_open; and not __fish_seen_argument -l timeout' -l timeout -r
complete -c cq -f -n '__cq_has_path service uninstall; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path service uninstall; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path service uninstall; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
complete -c cq -f -n '__cq_has_path version; and __cq_options_open; and not __fish_seen_argument -l help -s h' -l help -s h
complete -c cq -f -n '__cq_has_path version; and __cq_options_open; and not __fish_seen_argument -l json -s j' -l json -s j
complete -c cq -f -n '__cq_has_path version; and __cq_options_open; and not __fish_seen_argument -l version -s v' -l version -s v
