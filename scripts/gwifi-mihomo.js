// Clash Verge Rev global script. No Node/browser APIs are needed by main().
// Uses two wired entries and one Wi-Fi entry to assign NEW connections 2:1.
// This is connection balancing, not bonding of an existing TCP/QUIC connection.
// Prefer IPv4 without rejecting IPv6-only destinations. IPv6 reachability still
// depends on the campus network and the selected physical interface.
function main(config) {
  "use strict";

  const prefix = "GWIFI-";
  const selector = "GWIFI-DOWNLOAD";
  const physicalFilter = "^GWIFI-(Wired-A|Wired-B|WiFi)$";
  const nodes = [
    { name: "GWIFI-Wired-A", type: "direct", "interface-name": "以太网", "ip-version": "ipv4-prefer" },
    { name: "GWIFI-Wired-B", type: "direct", "interface-name": "以太网", "ip-version": "ipv4-prefer" },
    { name: "GWIFI-WiFi", type: "direct", "interface-name": "WLAN", "ip-version": "ipv4-prefer" }
  ];
  const groups = [
    {
      name: "GWIFI-Dual", type: "load-balance", strategy: "round-robin",
      proxies: ["GWIFI-Wired-A", "GWIFI-Wired-B", "GWIFI-WiFi"],
      url: "https://www.gstatic.cn/generate_204", "expected-status": 204,
      interval: 15, timeout: 4000, lazy: false
    },
    {
      name: selector, type: "select",
      // Start safely on wired; the installer enables dual only after checks.
      proxies: ["GWIFI-Wired-A", "GWIFI-Dual", "GWIFI-WiFi", "DIRECT"]
    }
  ];
  const localCIDRs = [
    "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8",
    "100.64.0.0/10", "169.254.0.0/16", "224.0.0.0/4"
  ];
  const guardRules = [
    "DOMAIN,localhost,DIRECT", "DOMAIN-SUFFIX,localhost,DIRECT",
    "DOMAIN-SUFFIX,local,DIRECT", "DOMAIN-SUFFIX,lan,DIRECT",
    "IP-CIDR,100.100.100.2/32,DIRECT,no-resolve"
  ].concat(localCIDRs.map(function (cidr) {
    return "IP-CIDR," + cidr + ",DIRECT,no-resolve";
  })).concat([
    "IP-CIDR6,::1/128,DIRECT,no-resolve", "IP-CIDR6,fc00::/7,DIRECT,no-resolve",
    "IP-CIDR6,fe80::/10,DIRECT,no-resolve", "IP-CIDR6,ff00::/8,DIRECT,no-resolve"
  ]);

  if (!config || typeof config !== "object" || Array.isArray(config)) {
    throw new Error("GWIFI: main(config) requires a configuration object");
  }
  ["proxies", "proxy-groups", "rules"].forEach(function (key) {
    if (config[key] != null && !Array.isArray(config[key])) {
      throw new Error("GWIFI: " + key + " must be an array");
    }
  });
  // Work on a copy: a rejected overlay must leave the incoming profile intact.
  const result = JSON.parse(JSON.stringify(config));
  const oldNodes = result.proxies || [];
  const oldGroups = result["proxy-groups"] || [];
  const oldRules = result.rules || [];

  function same(a, b) {
    if (a === b) return true;
    if (!a || !b || typeof a !== "object" || typeof b !== "object") return false;
    if (Array.isArray(a) !== Array.isArray(b)) return false;
    const ak = Object.keys(a).sort();
    const bk = Object.keys(b).sort();
    return ak.length === bk.length && ak.every(function (key, i) {
      return key === bk[i] && same(a[key], b[key]);
    });
  }

  const expected = nodes.concat(groups);
  const named = oldNodes.concat(oldGroups);
  const reserved = named.filter(function (entry) {
    return entry && typeof entry.name === "string" && entry.name.indexOf(prefix) === 0;
  });
  const providers = result["proxy-providers"] || {};
  if (Object.keys(providers).some(function (name) { return name.indexOf(prefix) === 0; })) {
    throw new Error("GWIFI: reserved GWIFI- name collides with a proxy provider");
  }
  const isRepeat = reserved.length === expected.length && expected.every(function (entry) {
    const matches = reserved.filter(function (old) { return old.name === entry.name; });
    return matches.length === 1 && same(matches[0], entry);
  }) && nodes.every(function (entry) {
    return oldNodes.some(function (old) { return old.name === entry.name; });
  }) && groups.every(function (entry) {
    return oldGroups.some(function (old) { return old.name === entry.name; });
  }) && guardRules.every(function (rule, i) { return oldRules[i] === rule; });
  if (reserved.length && !isRepeat) {
    throw new Error("GWIFI: reserved GWIFI- names already exist or the previous overlay was modified");
  }

  function excludePhysicalNodes(group) {
    if (group["include-all"] !== true && group["include-all-proxies"] !== true) return;
    const prior = group["exclude-filter"];
    if (prior != null && typeof prior !== "string") {
      throw new Error("GWIFI: exclude-filter must be a string");
    }
    // Mihomo splits these expressions on a backtick. Keep each existing
    // expression verbatim, including inline flags and its own alternations.
    const expressions = prior ? prior.split("`") : [];
    if (expressions.indexOf(physicalFilter) === -1) expressions.push(physicalFilter);
    group["exclude-filter"] = expressions.join("`");
  }

  const rewrittenGroups = oldGroups.filter(function (group) {
    return !isRepeat || group.name.indexOf(prefix) !== 0;
  }).map(function (group) {
    if (!group || typeof group !== "object" || Array.isArray(group)) {
      throw new Error("GWIFI: proxy-groups contains an invalid entry");
    }
    if (Array.isArray(group.proxies) && group.proxies.indexOf(selector) === -1) {
      const directIndex = group.proxies.indexOf("DIRECT");
      if (directIndex !== -1) group.proxies.splice(directIndex + 1, 0, selector);
    }
    excludePhysicalNodes(group);
    return group;
  });

  function ipv4CIDR(value) {
    const match = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d{1,2})$/.exec(value);
    if (!match || Number(match[5]) > 32) return null;
    let ip = 0;
    for (let i = 1; i <= 4; i++) {
      if (Number(match[i]) > 255) return null;
      ip = ip * 256 + Number(match[i]);
    }
    return { ip: ip, bits: Number(match[5]) };
  }
  function isPrivateDestination(type, payload, options) {
    const p = payload.trim().toLowerCase().replace(/\.$/, "");
    if (type === "DOMAIN" || type === "DOMAIN-SUFFIX") {
      return /(^|\.)(localhost|local|lan)$/.test(p);
    }
    if (type === "GEOIP" && p === "lan") return true;
    if (type === "GEOSITE" && p === "private") return true;
    if (type !== "IP-CIDR" || options.indexOf("src") !== -1) return false;
    const candidate = ipv4CIDR(p);
    if (!candidate) return false;
    return localCIDRs.some(function (cidr) {
      const network = ipv4CIDR(cidr);
      const block = Math.pow(2, 32 - network.bits);
      return candidate.bits >= network.bits &&
        Math.floor(candidate.ip / block) === Math.floor(network.ip / block);
    });
  }

  // Split only top-level commas. Logical conditions contain nested commas;
  // unknown or ambiguous syntax is deliberately left untouched.
  function ruleParts(rule) {
    let depth = 0;
    let start = 0;
    let escaped = false;
    const parts = [];
    for (let i = 0; i < rule.length; i++) {
      const char = rule[i];
      if (escaped) { escaped = false; continue; }
      if (char === "\\") { escaped = true; continue; }
      if (char === "(") depth++;
      if (char === ")") depth--;
      if (depth < 0) return null;
      if (char === "," && depth === 0) {
        parts.push(rule.slice(start, i));
        start = i + 1;
      }
    }
    if (depth !== 0) return null;
    parts.push(rule.slice(start));
    return parts;
  }
  const payloadTypes = (
    "DOMAIN DOMAIN-SUFFIX DOMAIN-KEYWORD DOMAIN-WILDCARD DOMAIN-REGEX GEOSITE " +
    "IP-CIDR IP-CIDR6 IP-SUFFIX IP-ASN GEOIP SRC-GEOIP SRC-IP-ASN SRC-IP-CIDR SRC-IP-SUFFIX " +
    "DST-PORT SRC-PORT IN-PORT IN-TYPE IN-USER IN-NAME REMATCH-NAME PROCESS-PATH " +
    "PROCESS-PATH-WILDCARD PROCESS-PATH-REGEX PROCESS-NAME PROCESS-NAME-WILDCARD " +
    "PROCESS-NAME-REGEX UID NETWORK DSCP RULE-SET AND OR NOT"
  ).split(" ");
  function rewriteRule(rule) {
    if (typeof rule !== "string") return rule;
    const parts = ruleParts(rule);
    if (!parts) return rule;
    const type = parts[0].trim().toUpperCase();
    const target = type === "MATCH" ? 1 : payloadTypes.indexOf(type) !== -1 ? 2 : -1;
    if (target < 0 || parts.length <= target || parts[target].trim() !== "DIRECT") return rule;
    const options = parts.slice(target + 1).map(function (part) { return part.trim(); });
    if (options.some(function (option) { return option !== "no-resolve" && option !== "src"; })) return rule;
    // IP-CIDR also accepts IPv6. Leave explicit IPv6 rules and sub-rule
    // dispatch targets intact; never prepend a blanket public IPv6 bypass.
    if (/\bIP-CIDR6\b/i.test(rule) ||
        ((type === "IP-CIDR" || type === "IP-SUFFIX" || type === "SRC-IP-CIDR" || type === "SRC-IP-SUFFIX") && parts[1].indexOf(":") !== -1) ||
        ((type === "AND" || type === "OR" || type === "NOT") && /\b(?:IP-CIDR|IP-SUFFIX)\s*,\s*[^,)]*:/i.test(parts[1]))) return rule;
    if (target === 2 && isPrivateDestination(type, parts[1], options)) return rule;
    parts[target] = parts[target].replace("DIRECT", selector);
    return parts.join(",");
  }

  result.proxies = oldNodes.filter(function (node) {
    return !isRepeat || node.name.indexOf(prefix) !== 0;
  }).concat(nodes);
  result["proxy-groups"] = rewrittenGroups.concat(groups);
  const sourceRules = isRepeat ? oldRules.slice(guardRules.length) : oldRules;
  result.rules = guardRules.concat(sourceRules.length ? sourceRules.map(rewriteRule) : ["MATCH," + selector]);
  return result;
}
