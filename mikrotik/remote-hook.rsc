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
# DNS static:       set comment to "hook:<param-name>"  (enabled=true → entry active)
#
# Domain blocking convention — one hook:<name> toggles a PAIR of rules:
#   1. /ip/dns/static add name=example.com type=NXDOMAIN match-subdomain=yes comment="hook:<name>"
#      (clients using router DNS stop resolving the domain; cache is flushed on toggle)
#   2. /ip/firewall/filter add chain=forward dst-address-list=<list> action=drop comment="hook:<name>"
#      where the address-list holds FQDN entries (/ip/firewall/address-list add list=<list> address=example.com)
#      — RouterOS resolves FQDNs dynamically, the existing firewall flow below
#      handles conntrack clearing + 30s temp-block of affected clients.

# --- Configuration (edit these) ---
:local url "http://your-server:8080/api/state"
:local token ""
:local scriptVersion "18"

# Set by applyRule when a /ip/dns/static entry actually changes state.
# Used after the main scan to flush DNS cache at most once per cycle.
:global remoteHookDnsFlushNeeded
:set remoteHookDnsFlushNeeded false
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

# --- Reentrancy lock: skip if a previous run is still applying changes ---
# With short scheduler intervals a slow conntrack sweep can outlive the
# interval and two runs race on the same rules/connections. Lock is the
# uptime of acquisition; a crashed run's stale lock expires after 90s
# (globals are also wiped on reboot, so no stale lock can survive one).
# Acquired after fetch/validation so a dead server never holds the lock.
:global remoteHookLock
:local nowUptime [/system/resource get uptime]
:if ([:typeof $remoteHookLock] = "time") do={
    :if (($nowUptime - $remoteHookLock) < 1m30s) do={
        :log info "remote-hook: previous run still active, skipping"
        :error "locked"
    }
}
:set remoteHookLock $nowUptime

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

# --- Helper: find conntrack entries where src/dst matches addr ---
# field: "src" or "dst". Plain IPs match exactly; CIDRs match by a regex
# prefix of the full octets covered by the mask, dots escaped
# ("172.16.0.0/16" -> "^172\.16\."). Non-octet-aligned masks round down to
# the nearest octet (slight over-match, acceptable for connection clearing).
:local findConns do={
    :local out [:toarray ""]
    :if ([:len $addr] = 0) do={ :return $out }
    :local slashPos [:find $addr "/"]
    :if ([:typeof $slashPos] != "num") do={
        :if ($field = "src") do={
            :do { :set out [/ip/firewall/connection find src-address=$addr] } on-error={}
        } else={
            :do { :set out [/ip/firewall/connection find dst-address=$addr] } on-error={}
        }
        :return $out
    }
    :local net [:pick $addr 0 $slashPos]
    :local mask [:tonum [:pick $addr ($slashPos + 1) [:len $addr]]]
    :local octets ($mask / 8)
    :if ($octets < 1) do={ :set octets 1 }
    :if ($octets > 4) do={ :set octets 4 }
    :local rx "^"
    :local rest $net
    :local i 0
    :while ($i < $octets) do={
        :local dotPos [:find $rest "."]
        :local part $rest
        :if ([:typeof $dotPos] = "num") do={
            :set part [:pick $rest 0 $dotPos]
            :set rest [:pick $rest ($dotPos + 1) [:len $rest]]
        }
        :if ($i > 0) do={ :set rx ($rx . "\\.") }
        :set rx ($rx . $part)
        :set i ($i + 1)
    }
    :if ($octets < 4) do={
        :set rx ($rx . "\\.")
    } else={
        # full IP: stop before the port (or end of string for ICMP)
        :set rx ($rx . "(:|\$)")
    }
    :if ($field = "src") do={
        :do { :set out [/ip/firewall/connection find src-address~"$rx"] } on-error={}
    } else={
        :do { :set out [/ip/firewall/connection find dst-address~"$rx"] } on-error={}
    }
    :return $out
}

# --- Helper: remove conntrack entries matching an address-list ---
# field: "src" or "dst". Returns the number of removed connections.
# Dynamic (FQDN-resolved) list entries can expire mid-loop — the get is
# guarded so a vanished entry is skipped instead of killing the script.
:local clearListConns do={
    :local total 0
    :foreach addrId in=[/ip/firewall/address-list find where list=$list && !disabled] do={
        :local addr ""
        :do { :set addr [:tostr [/ip/firewall/address-list get $addrId address]] } on-error={}
        :if ([:len $addr] > 0) do={
            :local connIds [$findConns addr=$addr field=$field]
            :if ([:len $connIds] > 0) do={
                :do { /ip/firewall/connection remove $connIds } on-error={}
                :set total ($total + [:len $connIds])
            }
        }
    }
    :return $total
}

# --- Helper: append item to array unless already present ---
:local appendUnique do={
    :foreach a in=$arr do={
        :if ($a = $item) do={ :return $arr }
    }
    :return ($arr , $item)
}

# --- Helper: apply enable/disable to a rule ---
:local applyRule do={
    :local currentDisabled
    :do {
        :set currentDisabled [[:parse ":return [$section get $ruleId disabled]"]]
    } on-error={}
    :if ($shouldEnable = true) do={
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
                    :foreach addrId in=[/ip/firewall/address-list find where list=$srcList && !disabled] do={
                        :local addr ""
                        :do { :set addr [:tostr [/ip/firewall/address-list get $addrId address]] } on-error={}
                        :local slashPos [:find $addr "/"]
                        :if ([:typeof $slashPos] = "num") do={ :set addr [:pick $addr 0 $slashPos] }
                        :if ([:len $addr] > 0) do={
                            :set preCollectedSrc [$appendUnique arr=$preCollectedSrc item=$addr]
                        }
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
                    :foreach addrId in=[/ip/firewall/address-list find where list=$dstList && !disabled] do={
                        :local addr ""
                        :do { :set addr [:tostr [/ip/firewall/address-list get $addrId address]] } on-error={}
                        :foreach connId in=[$findConns addr=$addr field="dst"] do={
                            :do {
                                :local srcIp [:tostr [/ip/firewall/connection get $connId src-address]]
                                # conntrack address may carry ":port" — keep the IP only
                                :local colonPos [:find $srcIp ":"]
                                :if ([:typeof $colonPos] = "num") do={ :set srcIp [:pick $srcIp 0 $colonPos] }
                                :if ([:len $srcIp] > 0) do={
                                    :set preCollectedSrc [$appendUnique arr=$preCollectedSrc item=$srcIp]
                                }
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

            :if ($enableOk && [:find $section "dns"] != nothing) do={
                :global remoteHookDnsFlushNeeded
                :set remoteHookDnsFlushNeeded true
            }

            # Clear connection tracking only if rule was successfully enabled
            :if ($enableOk && [:find $section "firewall"] != nothing) do={
                :local totalCleared 0

                # Kill existing connections matching the rule's address-lists
                :if ($hasSrcList) do={
                    :set totalCleared ($totalCleared + [$clearListConns list=$srcList field="src" findConns=$findConns])
                }
                :if ($hasDstList) do={
                    :set totalCleared ($totalCleared + [$clearListConns list=$dstList field="dst" findConns=$findConns])
                }

                # Temp-block pre-collected src IPs for 30s, then kill all their connections
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
                        :do {
                            :if ([:len $estRule] > 0) do={
                                /ip/firewall/filter add chain=forward src-address-list=_temp-block action=drop comment="hook:_temp-block" place-before=($estRule->0)
                            } else={
                                /ip/firewall/filter add chain=forward src-address-list=_temp-block action=drop comment="hook:_temp-block" place-before=0
                            }
                            :log info "remote-hook: created _temp-block drop rule"
                        } on-error={
                            :log warning "remote-hook: failed to create _temp-block drop rule"
                        }
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
                            :local allConns [$findConns addr=$srcIp field="src"]
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
        :if ($currentDisabled = false) do={
            :local disableOk false
            :do {
                [[:parse "$section set $ruleId disabled=yes"]]
                :set disableOk true
                :log info "remote-hook: disabled $paramName in $section"
            } on-error={
                :log warning "remote-hook: failed to disable $paramName in $section"
            }
            :if ($disableOk && [:find $section "dns"] != nothing) do={
                :global remoteHookDnsFlushNeeded
                :set remoteHookDnsFlushNeeded true
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
    "/ip/kid-control";
    "/ip/dns/static"
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
                    [$applyRule section=$section ruleId=$ruleId paramName=$paramName shouldEnable=$shouldEnable findConns=$findConns clearListConns=$clearListConns appendUnique=$appendUnique]
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
        [$processRules rules=$rulesByComment section=$section field="comment" inverted=$inverted lookupEnabled=$lookupEnabled applyRule=$applyRule findConns=$findConns clearListConns=$clearListConns appendUnique=$appendUnique content=$content]
    }

    # --- 2. Search by name field (kid-control etc.) ---
    :local rulesByName
    :do {
        :set rulesByName [[:parse ":return [$section find where name~\"hook:\"]"]]
    } on-error={}
    :if ([:typeof $rulesByName] = "array") do={
        [$processRules rules=$rulesByName section=$section field="name" inverted=$inverted lookupEnabled=$lookupEnabled applyRule=$applyRule findConns=$findConns clearListConns=$clearListConns appendUnique=$appendUnique content=$content]
    }
}

# --- Flush DNS cache once if any /ip/dns/static entry changed ---
:if ($remoteHookDnsFlushNeeded = true) do={
    :do {
        /ip/dns/cache/flush
        :log info "remote-hook: flushed DNS cache after dns/static toggle"
    } on-error={
        :log warning "remote-hook: failed to flush DNS cache"
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

# Release the reentrancy lock (a crashed run's lock expires by itself)
:set remoteHookLock

:log info "remote-hook: sync completed"
