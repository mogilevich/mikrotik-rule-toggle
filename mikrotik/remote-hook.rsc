# MikroTik Remote Hook Script (RouterOS 7)
# Fetches parameter state from a remote server and enables/disables
# rules tagged with "hook:<param-name>" in their comment or name fields.
#
# Setup:
#   1. Edit url and token below, then import:
#        /tool/fetch url="http://your-server:8080/mikrotik/remote-hook.rsc" dst-path=remote-hook.rsc
#        /system/script add name=remote-hook source=[/file/get remote-hook.rsc contents]
#   2. Create scheduler:
#        /system/scheduler add name=remote-hook interval=1m on-event="/system/script/run remote-hook"
#
# Firewall rules:   set comment to "hook:<param-name>"  (enabled=true → rule active → blocks traffic)
# Kid-control:      set name to "hook:<param-name>"     (enabled=true → rule disabled → child unrestricted)
#                   Kid-control has INVERTED logic: disabling the kid-control rule
#                   removes schedule restrictions, giving the child full access.

# --- Configuration (edit these) ---
:local url "http://10.38.10.11:8080/api/state"
:local token ""

# --- Fetch state from server (in memory, no disk writes) ---
:local content ""

:do {
    :if ($token != "") do={
        :set content ([/tool/fetch url=$url http-header-field="Authorization: Bearer $token" output=user as-value]->"data")
    } else={
        :set content ([/tool/fetch url=$url output=user as-value]->"data")
    }
} on-error={
    :log warning "remote-hook: failed to fetch state from $url"
    :error "fetch failed"
}

# --- Helper: look up param enabled state in JSON ---
# Returns true/false/nil (nil = param not found)
:local lookupEnabled do={
    :local searchStr ("\"$paramName\"")
    :local paramPos [:find $content $searchStr]
    :if ([:typeof $paramPos] != "num") do={ :return nothing }
    :local enabledPos [:find $content "\"enabled\"" $paramPos]
    :if ([:typeof $enabledPos] != "num") do={ :return nothing }
    :local truePos [:find $content "true" $enabledPos]
    :local falsePos [:find $content "false" $enabledPos]
    :if ([:typeof $truePos] = "num" && [:typeof $falsePos] = "num") do={
        :if ($truePos < $falsePos) do={ :return true }
        :return false
    }
    :if ([:typeof $truePos] = "num") do={ :return true }
    :return false
}

# --- Helper: apply enable/disable to a rule ---
:local applyRule do={
    :if ($shouldEnable = true) do={
        :local currentDisabled
        :do {
            :set currentDisabled [[:parse ":return [$section get $ruleId disabled]"]]
        } on-error={}
        :if ($currentDisabled = true) do={
            :do {
                [[:parse "$section set $ruleId disabled=no"]]
                :log info "remote-hook: enabled $paramName in $section"
            } on-error={
                :log warning "remote-hook: failed to enable $paramName in $section"
            }
        }
    } else={
        :local currentDisabled
        :do {
            :set currentDisabled [[:parse ":return [$section get $ruleId disabled]"]]
        } on-error={}
        :if ($currentDisabled = false) do={
            :do {
                [[:parse "$section set $ruleId disabled=yes"]]
                :log info "remote-hook: disabled $paramName in $section"
            } on-error={
                :log warning "remote-hook: failed to disable $paramName in $section"
            }
        }
    }
}

# --- Sections to scan ---
# Normal logic:   enabled=true in web → disabled=no on MikroTik (rule active)
# Inverted logic: enabled=true in web → disabled=yes on MikroTik (rule inactive)
:local sections {
    "/ip/firewall/filter";
    "/ip/firewall/nat";
    "/ip/firewall/mangle";
    "/ip/kid-control"
}

# Sections with inverted logic (kid-control: disabling rule = removing restrictions)
:local invertedSections {
    "/ip/kid-control"
}

# --- Helper: check if section has inverted logic ---
:local isInverted do={
    :foreach s in=$invertedSections do={
        :if ($s = $section) do={ :return true }
    }
    :return false
}

# --- Helper: process found rules ---
:local processRules do={
    :foreach ruleId in=$rules do={
        :local tagValue
        :do {
            :set tagValue [[:parse ":return [$section get $ruleId $field]"]]
        } on-error={}

        :if ([:typeof $tagValue] = "str") do={
            :local hookPos [:find $tagValue "hook:"]
            :if ([:typeof $hookPos] = "num") do={
                :local paramStart ($hookPos + 5)
                :local paramEnd [:find $tagValue " " $paramStart]
                :local paramName
                :if ([:typeof $paramEnd] = "num") do={
                    :set paramName [:pick $tagValue $paramStart $paramEnd]
                } else={
                    :set paramName [:pick $tagValue $paramStart [:len $tagValue]]
                }
                :local shouldEnable [$lookupEnabled paramName=$paramName content=$content]
                :if ([:typeof $shouldEnable] = "bool") do={
                    # Invert logic for kid-control sections
                    :if ($inverted = true) do={
                        :if ($shouldEnable = true) do={
                            :set shouldEnable false
                        } else={
                            :set shouldEnable true
                        }
                    }
                    [$applyRule section=$section ruleId=$ruleId paramName=$paramName shouldEnable=$shouldEnable]
                }
            }
        }
    }
}

# --- Scan each section ---
:foreach section in=$sections do={
    :local inverted [$isInverted section=$section invertedSections=$invertedSections]

    # --- 1. Search by comment field (firewall rules etc.) ---
    :local rulesByComment
    :do {
        :set rulesByComment [[:parse ":return [$section find where comment~\"hook:\"]"]]
    } on-error={}
    :if ([:typeof $rulesByComment] = "array") do={
        [$processRules rules=$rulesByComment section=$section field="comment" inverted=$inverted lookupEnabled=$lookupEnabled applyRule=$applyRule content=$content]
    }

    # --- 2. Search by name field (kid-control etc.) ---
    :local rulesByName
    :do {
        :set rulesByName [[:parse ":return [$section find where name~\"hook:\"]"]]
    } on-error={}
    :if ([:typeof $rulesByName] = "array") do={
        [$processRules rules=$rulesByName section=$section field="name" inverted=$inverted lookupEnabled=$lookupEnabled applyRule=$applyRule content=$content]
    }
}

:log info "remote-hook: sync completed"
