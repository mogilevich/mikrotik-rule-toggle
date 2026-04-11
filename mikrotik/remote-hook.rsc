# MikroTik Remote Hook Script (RouterOS 7)
# Fetches parameter state from a remote server and enables/disables
# rules tagged with "hook:<param-name>" in their comments.
#
# Setup:
#   1. Set global variables (or edit below):
#        /system/script/environment add name=hookUrl value="http://your-server:8080/api/state"
#        /system/script/environment add name=hookToken value="your-token"
#   2. Import this script:
#        /system/script add name=remote-hook source=[/file/get remote-hook.rsc contents]
#   3. Create scheduler:
#        /system/scheduler add name=remote-hook interval=1m on-event="/system/script/run remote-hook"
#
# Tag your rules with comments like:  hook:block-social  hook:kid-control  etc.

# --- Configuration ---
:local url "http://your-server:8080/api/state"
:local token ""

# Try to read from global environment variables
:do {
    :set url [/system/script/environment get [find name=hookUrl] value]
} on-error={}
:do {
    :set token [/system/script/environment get [find name=hookToken] value]
} on-error={}

# --- Fetch state from server ---
:local fetchFile "hook-state.txt"

:do {
    :if ($token != "") do={
        /tool/fetch url=$url http-header-field="Authorization: Bearer $token" output=file dst-path=$fetchFile
    } else={
        /tool/fetch url=$url output=file dst-path=$fetchFile
    }
} on-error={
    :log warning "remote-hook: failed to fetch state from $url"
    :error "fetch failed"
}

:delay 1s
:local content [/file/get [find name=$fetchFile] contents]
/file/remove [find name=$fetchFile]

# --- Sections to scan for hook comments ---
:local sections {
    "/ip/firewall/filter";
    "/ip/firewall/nat";
    "/ip/firewall/mangle";
    "/ip/kid-control"
}

# --- Parse and apply each parameter ---
# JSON format: {"params":{"name":{"enabled":true/false,...},...}}
# We parse by searching for each "hook:xxx" comment in our sections,
# then finding whether that param is enabled or disabled in the response.

:foreach section in=$sections do={
    :local rules
    :do {
        :set rules [[:parse ":return [$section find where comment~\"hook:\"]"]]
    } on-error={
        :log debug "remote-hook: section $section not available"
    }

    :if ([:typeof $rules] = "array") do={
        :foreach ruleId in=$rules do={
            :local comment
            :do {
                :set comment [[:parse ":return [$section get $ruleId comment]"]]
            } on-error={}

            :if ([:typeof $comment] = "str") do={
                # Extract param name from comment: find "hook:" prefix
                :local hookPos [:find $comment "hook:"]
                :if ([:typeof $hookPos] = "num") do={
                    :local paramStart ($hookPos + 5)
                    :local paramEnd [:find $comment " " $paramStart]
                    :local paramName
                    :if ([:typeof $paramEnd] = "num") do={
                        :set paramName [:pick $comment $paramStart $paramEnd]
                    } else={
                        :set paramName [:pick $comment $paramStart [:len $comment]]
                    }

                    # Look up this param in the fetched content
                    :local searchStr ("\"$paramName\"")
                    :local paramPos [:find $content $searchStr]

                    :if ([:typeof $paramPos] = "num") do={
                        # Find "enabled" value after param name
                        :local enabledPos [:find $content "\"enabled\"" $paramPos]
                        :if ([:typeof $enabledPos] = "num") do={
                            :local truePos [:find $content "true" $enabledPos]
                            :local falsePos [:find $content "false" $enabledPos]
                            :local shouldEnable false

                            :if ([:typeof $truePos] = "num" && [:typeof $falsePos] = "num") do={
                                :if ($truePos < $falsePos) do={ :set shouldEnable true }
                            } else={
                                :if ([:typeof $truePos] = "num") do={ :set shouldEnable true }
                            }

                            :local currentDisabled
                            :do {
                                :set currentDisabled [[:parse ":return [$section get $ruleId disabled]"]]
                            } on-error={}

                            :if ($shouldEnable && $currentDisabled = true) do={
                                :do {
                                    [[:parse "$section set $ruleId disabled=no"]]
                                    :log info "remote-hook: enabled $paramName in $section"
                                } on-error={
                                    :log warning "remote-hook: failed to enable $paramName in $section"
                                }
                            }
                            :if (!$shouldEnable && $currentDisabled = false) do={
                                :do {
                                    [[:parse "$section set $ruleId disabled=yes"]]
                                    :log info "remote-hook: disabled $paramName in $section"
                                } on-error={
                                    :log warning "remote-hook: failed to disable $paramName in $section"
                                }
                            }
                        }
                    }
                }
            }
        }
    }
}

:log info "remote-hook: sync completed"
