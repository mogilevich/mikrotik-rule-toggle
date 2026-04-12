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
:local url "http://your-server:8080/api/state"
:local token ""
:local scriptVersion "11"
:local scriptName "remote-hook"

# --- Fetch state from server (in memory, no disk writes) ---
:local content ""

:do {
    :if ($token != "") do={
        :set content ([/tool/fetch url=$url http-header-field="Authorization: Bearer $token,X-Script-Version: $scriptVersion" output=user as-value duration=10]->"data")
    } else={
        :set content ([/tool/fetch url=$url http-header-field="X-Script-Version: $scriptVersion" output=user as-value duration=10]->"data")
    }
} on-error={
    :log warning "remote-hook: failed to fetch state from $url"
    :error "fetch failed"
}

# Validate response contains expected JSON
:if ([:len $content] = 0) do={
    :log warning "remote-hook: empty response from server"
    :error "empty response"
}
:if ([:typeof [:find $content "\"params\""]] != "num") do={
    :log warning "remote-hook: invalid response (no params key)"
    :error "invalid response"
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
            # Pre-collect src IPs for temp-block BEFORE enabling the rule
            :local srcList ""
            :local srcAddr ""
            :local dstList ""
            :local preCollectedSrc [:toarray ""]
            :local hasSrcList false
            :local hasSrcAddr false
            :local hasDstList false
            :if ([:find $section "firewall"] != nothing) do={
                :do { :set srcList [[:parse ":return [$section get $ruleId src-address-list]"]] } on-error={}
                :do { :set srcAddr [[:parse ":return [$section get $ruleId src-address]"]] } on-error={}
                :do { :set dstList [[:parse ":return [$section get $ruleId dst-address-list]"]] } on-error={}
                :set hasSrcList ([:typeof $srcList] = "str" && [:len $srcList] > 0)
                :set hasSrcAddr ([:typeof $srcAddr] = "str" && [:len $srcAddr] > 0)
                :set hasDstList ([:typeof $dstList] = "str" && [:len $dstList] > 0)

                # Priority 1: src-address-list → resolve to IPs
                :if ($hasSrcList) do={
                    :local addrIds [/ip/firewall/address-list find list=$srcList]
                    :foreach addrId in=$addrIds do={
                        :local addr [/ip/firewall/address-list get $addrId address]
                        :local slashPos [:find $addr "/"]
                        :if ([:typeof $slashPos] = "num") do={ :set addr [:pick $addr 0 $slashPos] }
                        :local found false
                        :foreach a in=$preCollectedSrc do={
                            :if ($a = $addr) do={ :set found true }
                        }
                        :if (!$found) do={ :set preCollectedSrc ($preCollectedSrc , $addr) }
                    }
                }
                # Priority 2: src-address → use directly
                :if ($hasSrcAddr && !$hasSrcList) do={
                    :local addr $srcAddr
                    :local slashPos [:find $addr "/"]
                    :if ([:typeof $slashPos] = "num") do={ :set addr [:pick $addr 0 $slashPos] }
                    :set preCollectedSrc ($preCollectedSrc , $addr)
                }
                # Priority 3: no src info → scan conntrack by dst-address-list
                :if (!$hasSrcList && !$hasSrcAddr && $hasDstList) do={
                    :local addrIds [/ip/firewall/address-list find list=$dstList]
                    :foreach addrId in=$addrIds do={
                        :local addr [/ip/firewall/address-list get $addrId address]
                        :local isCidr false
                        :local slashPos [:find $addr "/"]
                        :if ([:typeof $slashPos] = "num") do={
                            :set addr [:pick $addr 0 $slashPos]
                            :set isCidr true
                        }
                        :local connIds
                        :if ($isCidr) do={
                            :set connIds [/ip/firewall/connection find dst-address~"^$addr"]
                        } else={
                            :set connIds [/ip/firewall/connection find dst-address=$addr]
                        }
                        :foreach connId in=$connIds do={
                            :do {
                                :local srcIp [/ip/firewall/connection get $connId src-address]
                                :local found false
                                :foreach a in=$preCollectedSrc do={
                                    :if ($a = $srcIp) do={ :set found true }
                                }
                                :if (!$found) do={ :set preCollectedSrc ($preCollectedSrc , $srcIp) }
                            } on-error={}
                        }
                    }
                }
            }

            # Enable the rule
            :local enableOk false
            :do {
                [[:parse "$section set $ruleId disabled=no"]]
                :set enableOk true
                :log info "remote-hook: enabled $paramName in $section"
            } on-error={
                :log warning "remote-hook: failed to enable $paramName in $section"
            }

            # Clear connection tracking only if rule was successfully enabled
            :if ($enableOk && [:find $section "firewall"] != nothing) do={
                :local totalCleared 0

                # Clear connections matching src-address-list
                :if ($hasSrcList) do={
                    :local addrIds [/ip/firewall/address-list find list=$srcList]
                    :foreach addrId in=$addrIds do={
                        :local addr [/ip/firewall/address-list get $addrId address]
                        :local isCidr false
                        :local slashPos [:find $addr "/"]
                        :if ([:typeof $slashPos] = "num") do={
                            :set addr [:pick $addr 0 $slashPos]
                            :set isCidr true
                        }
                        :local connIds
                        :if ($isCidr) do={
                            :set connIds [/ip/firewall/connection find src-address~"^$addr"]
                        } else={
                            :set connIds [/ip/firewall/connection find src-address=$addr]
                        }
                        :if ([:len $connIds] > 0) do={
                            :do { /ip/firewall/connection remove $connIds } on-error={}
                            :set totalCleared ($totalCleared + [:len $connIds])
                        }
                    }
                }

                # Clear dst-list connections
                :if ($hasDstList) do={
                    :local addrIds [/ip/firewall/address-list find list=$dstList]
                    :foreach addrId in=$addrIds do={
                        :local addr [/ip/firewall/address-list get $addrId address]
                        :local isCidr false
                        :local slashPos [:find $addr "/"]
                        :if ([:typeof $slashPos] = "num") do={
                            :set addr [:pick $addr 0 $slashPos]
                            :set isCidr true
                        }
                        :local connIds
                        :if ($isCidr) do={
                            :set connIds [/ip/firewall/connection find dst-address~"^$addr"]
                        } else={
                            :set connIds [/ip/firewall/connection find dst-address=$addr]
                        }
                        :if ([:len $connIds] > 0) do={
                            :do { /ip/firewall/connection remove $connIds } on-error={}
                            :set totalCleared ($totalCleared + [:len $connIds])
                        }
                    }
                }

                # Temp-block pre-collected src IPs for 1m, then kill all their connections
                # Check if we have real IPs (not just empty initial element)
                :local hasRealSrc false
                :foreach chk in=$preCollectedSrc do={
                    :if ([:len $chk] > 0) do={ :set hasRealSrc true }
                }
                :if ($hasRealSrc) do={
                    # Ensure drop rule exists before established/related accept
                    :local tbRule [/ip/firewall/filter find comment="hook:_temp-block"]
                    :local estRule [/ip/firewall/filter find where chain=forward connection-state~"established"]
                    :if ([:len $tbRule] = 0) do={
                        :if ([:len $estRule] > 0) do={
                            /ip/firewall/filter add chain=forward src-address-list=_temp-block action=drop comment="hook:_temp-block" place-before=($estRule->0)
                        } else={
                            /ip/firewall/filter add chain=forward src-address-list=_temp-block action=drop comment="hook:_temp-block" place-before=0
                        }
                        :log info "remote-hook: created _temp-block drop rule"
                    } else={
                        :if ([:len $estRule] > 0) do={
                            :do { /ip/firewall/filter move ($tbRule->0) ($estRule->0) } on-error={}
                        }
                    }
                    :foreach srcIp in=$preCollectedSrc do={
                        :if ([:len $srcIp] > 0) do={
                            :do {
                                /ip/firewall/address-list add list=_temp-block address=$srcIp timeout=30s
                                :log info "remote-hook: temp-blocked $srcIp for 30s ($paramName)"
                            } on-error={}
                            :local allConns [/ip/firewall/connection find src-address=$srcIp]
                            :if ([:len $allConns] > 0) do={
                                :do { /ip/firewall/connection remove $allConns } on-error={}
                                :set totalCleared ($totalCleared + [:len $allConns])
                            }
                        }
                    }
                }

                :if ($totalCleared > 0) do={
                    :log info "remote-hook: cleared $totalCleared connections for $paramName"
                }
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

# --- Auto-update script if server signals new version ---
:local updatePos [:find $content "\"script_update\""]
:if ([:typeof $updatePos] = "num") do={
    :local trueCheck [:find $content "true" $updatePos]
    :if ([:typeof $trueCheck] != "num") do={ :set updatePos nothing }
}
:if ([:typeof $updatePos] = "num") do={
    :log info "remote-hook: server signals script update, downloading new version"
    # Derive base URL from api/state URL
    :local baseUrl [:pick $url 0 [:find $url "/api/state"]]
    :local rscUrl "$baseUrl/mikrotik/remote-hook.rsc"
    :local newScript ""
    :do {
        :set newScript ([/tool/fetch url=$rscUrl output=user as-value duration=10]->"data")
    } on-error={
        :log warning "remote-hook: failed to download updated script"
    }
    :if ([:len $newScript] > 0) do={
        :do {
            /system/script set $scriptName source=$newScript
            :log info "remote-hook: script updated successfully"
        } on-error={
            :log warning "remote-hook: failed to update script source"
        }
    }
}

:log info "remote-hook: sync completed"
