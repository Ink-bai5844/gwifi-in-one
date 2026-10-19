"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const context = vm.createContext({});
vm.runInContext(fs.readFileSync(path.join(__dirname, "gwifi-mihomo.js"), "utf8"), context);
const apply = (config) => JSON.parse(JSON.stringify(context.main(config)));
const selector = "GWIFI-DOWNLOAD";
const group = (config, name) => config["proxy-groups"].find((entry) => entry.name === name);
const reserved = (config) => config.proxies.concat(config["proxy-groups"]).filter((entry) => entry.name.startsWith("GWIFI-"));

test("works without proxy nodes and preserves DNS/TUN/global settings", () => {
  const input = { dns: { enable: true, "enhanced-mode": "fake-ip" }, tun: { enable: true }, mode: "rule" };
  const result = apply(input);
  assert.equal(result.proxies.length, 3);
  assert.deepEqual(result.proxies.map((p) => [p.name, p["interface-name"], p["ip-version"]]), [
    ["GWIFI-Wired-A", "以太网", "ipv4-prefer"], ["GWIFI-Wired-B", "以太网", "ipv4-prefer"], ["GWIFI-WiFi", "WLAN", "ipv4-prefer"]
  ]);
  assert.deepEqual(result.dns, input.dns);
  assert.deepEqual(result.tun, input.tun);
  assert.equal(result.mode, input.mode);
  assert.equal(result.rules.at(-1), "MATCH,GWIFI-DOWNLOAD");
  assert.equal(input.proxies, undefined, "input was not mutated");
  assert.deepEqual(group(result, selector).proxies, ["GWIFI-Wired-A", "GWIFI-Dual", "GWIFI-WiFi", "DIRECT"]);
  const dual = group(result, "GWIFI-Dual");
  assert.equal(dual.strategy, "round-robin");
  assert.deepEqual(dual.proxies, ["GWIFI-Wired-A", "GWIFI-Wired-B", "GWIFI-WiFi"]);
  assert.equal(dual["expected-status"], 204);
  assert.equal(dual.interval, 15);
  assert.equal(dual.timeout, 4000);
  assert.equal(dual.lazy, false);
});

test("retains foreign proxies and existing selector/default selection order", () => {
  const input = {
    proxies: [{ name: "Overseas", type: "socks5", server: "example.test", port: 1080 }],
    "proxy-groups": [
      { name: "Select", type: "select", proxies: ["Overseas", "DIRECT"], "default-selected": "Overseas" },
      { name: "Domestic", type: "select", proxies: ["DIRECT", "Select"], "default-selected": "DIRECT" }
    ],
    rules: ["DOMAIN-SUFFIX,example.org,Select", "GEOIP,CN,Domestic", "MATCH,Select"]
  };
  const original = structuredClone(input);
  const result = apply(input);
  assert.deepEqual(input, original);
  assert.deepEqual(result.proxies[0], input.proxies[0]);
  assert.deepEqual(group(result, "Select").proxies, ["Overseas", "DIRECT", selector]);
  assert.deepEqual(group(result, "Domestic").proxies, ["DIRECT", selector, "Select"]);
  assert.equal(group(result, "Select")["default-selected"], "Overseas");
  assert.equal(group(result, "Domestic")["default-selected"], "DIRECT");
  assert.deepEqual(result.rules.slice(-3), input.rules);
});

test("protects LAN, campus portal and existing private destination rules", () => {
  const existing = [
    "DOMAIN-SUFFIX,router.lan,DIRECT", "DOMAIN,device.local,DIRECT", "GEOIP,LAN,DIRECT",
    "GEOSITE,private,DIRECT", "IP-CIDR,10.1.2.0/24,DIRECT,no-resolve", "IP-CIDR,172.31.0.0/16,DIRECT",
    "IP-CIDR,100.100.100.2/32,DIRECT", "IP-CIDR,192.168.1.1/32,DIRECT",
    "IP-CIDR6,fd00::/64,DIRECT,no-resolve", "MATCH,DIRECT"
  ];
  const result = apply({ rules: existing });
  assert.deepEqual(result.rules.slice(-existing.length, -1), existing.slice(0, -1));
  for (const rule of [
    "DOMAIN-SUFFIX,lan,DIRECT", "IP-CIDR,100.100.100.2/32,DIRECT,no-resolve",
    "IP-CIDR,10.0.0.0/8,DIRECT,no-resolve", "IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
    "IP-CIDR,192.168.0.0/16,DIRECT,no-resolve", "IP-CIDR,100.64.0.0/10,DIRECT,no-resolve",
    "IP-CIDR,169.254.0.0/16,DIRECT,no-resolve", "IP-CIDR,224.0.0.0/4,DIRECT,no-resolve",
    "IP-CIDR6,::1/128,DIRECT,no-resolve", "IP-CIDR6,fc00::/7,DIRECT,no-resolve",
    "IP-CIDR6,fe80::/10,DIRECT,no-resolve", "IP-CIDR6,ff00::/8,DIRECT,no-resolve"
  ]) assert.ok(result.rules.indexOf(rule) < result.rules.length - existing.length, rule);
  assert.ok(!result.rules.includes("IP-CIDR6,::/0,DIRECT,no-resolve"));
  assert.ok(!result.rules.includes("NETWORK,UDP,DIRECT"));
});

test("rewrites only parsed DIRECT targets, with logical rules and options", () => {
  const cases = [
    ["MATCH,DIRECT", "MATCH,GWIFI-DOWNLOAD"],
    ["DOMAIN,DIRECT,Proxy", "DOMAIN,DIRECT,Proxy"],
    ["RULE-SET,DIRECT,Proxy", "RULE-SET,DIRECT,Proxy"],
    ["SUB-RULE,(NETWORK,tcp),DIRECT", "SUB-RULE,(NETWORK,tcp),DIRECT"],
    ["RULE-SET,Domestic,DIRECT,no-resolve", "RULE-SET,Domestic,GWIFI-DOWNLOAD,no-resolve"],
    ["IP-CIDR,8.8.8.0/24,DIRECT,no-resolve,src", "IP-CIDR,8.8.8.0/24,GWIFI-DOWNLOAD,no-resolve,src"],
    ["IP-CIDR,10.1.0.0/16,DIRECT,src", "IP-CIDR,10.1.0.0/16,GWIFI-DOWNLOAD,src"],
    ["SRC-IP-CIDR,192.168.0.0/16,DIRECT", "SRC-IP-CIDR,192.168.0.0/16,GWIFI-DOWNLOAD"],
    ["AND,((NETWORK,UDP),(DST-PORT,443)),DIRECT", "AND,((NETWORK,UDP),(DST-PORT,443)),GWIFI-DOWNLOAD"],
    ["OR,((DOMAIN,foo.test),(DOMAIN,bar.test)),DIRECT", "OR,((DOMAIN,foo.test),(DOMAIN,bar.test)),GWIFI-DOWNLOAD"],
    ["NOT,((NETWORK,UDP)),DIRECT", "NOT,((NETWORK,UDP)),GWIFI-DOWNLOAD"],
    ["PROCESS-PATH,C:\\Downloads\\DIRECT.exe,DIRECT", "PROCESS-PATH,C:\\Downloads\\DIRECT.exe,GWIFI-DOWNLOAD"],
    ["DOMAIN,example.com, DIRECT ", "DOMAIN,example.com, GWIFI-DOWNLOAD "],
    ["NEW-RULE,DIRECT", "NEW-RULE,DIRECT"],
    ["DOMAIN,example.com,DIRECT,unknown-option", "DOMAIN,example.com,DIRECT,unknown-option"],
    ["DOMAIN-REGEX,^abc[),]$,DIRECT", "DOMAIN-REGEX,^abc[),]$,DIRECT"]
  ];
  const result = apply({ rules: cases.map(([before]) => before), "sub-rules": { DIRECT: ["MATCH,Overseas"] } });
  assert.deepEqual(result.rules.slice(-cases.length), cases.map(([, after]) => after));
  assert.deepEqual(result["sub-rules"], { DIRECT: ["MATCH,Overseas"] });
});

test("keeps explicit public IPv6 and proxy rules in their original order", () => {
  const rules = [
    "DOMAIN-SUFFIX,foreign.test,Overseas",
    "IP-CIDR6,2001:4860::/32,DIRECT", "IP-CIDR,2001:4860::/32,DIRECT",
    "AND,((NETWORK,TCP),(IP-CIDR,2001:4860::/32)),DIRECT",
    "IP-CIDR6,::/0,Overseas", "MATCH,DIRECT"
  ];
  const result = apply({ rules });
  assert.deepEqual(result.rules.slice(-rules.length, -1), rules.slice(0, -1));
  assert.equal(result.rules.at(-1), "MATCH,GWIFI-DOWNLOAD");
});

test("private CIDR protection does not swallow neighboring public ranges or broad rules", () => {
  const input = [
    "IP-CIDR,172.15.255.255/32,DIRECT", "IP-CIDR,172.32.0.0/16,DIRECT",
    "IP-CIDR,100.63.255.255/32,DIRECT", "IP-CIDR,100.128.0.0/16,DIRECT",
    "IP-CIDR,10.0.0.0/7,DIRECT", "IP-CIDR,0.0.0.0/0,DIRECT",
    "DOMAIN-SUFFIX,notlocal,DIRECT", "DOMAIN-SUFFIX,lan.example.com,DIRECT"
  ];
  assert.deepEqual(apply({ rules: input }).rules.slice(-input.length), input.map((rule) => rule.replace(/DIRECT$/, selector)));
});

test("include-all excludes only physical GWIFI nodes and preserves original regex expressions", () => {
  const input = {
    "proxy-groups": [
      { name: "All", type: "select", "include-all": true, "exclude-filter": "(?i)expired|剩余`^Trial$", proxies: ["DIRECT"] },
      { name: "AllProxies", type: "select", "include-all-proxies": true },
      { name: "Providers", type: "select", "include-all-providers": true, "exclude-filter": "foo" }
    ]
  };
  const result = apply(input);
  const filter = "^GWIFI-(Wired-A|Wired-B|WiFi)$";
  assert.equal(group(result, "All")["exclude-filter"], "(?i)expired|剩余`^Trial$`" + filter);
  assert.equal(group(result, "AllProxies")["exclude-filter"], filter);
  assert.equal(group(result, "Providers")["exclude-filter"], "foo");
  assert.ok(new RegExp(filter).test("GWIFI-Wired-B"));
  assert.ok(!new RegExp(filter).test(selector));
  assert.deepEqual(group(result, "All").proxies, ["DIRECT", selector]);
});

test("applying the overlay repeatedly adds no nodes, rules, choices or filters", () => {
  const input = { "proxy-groups": [{ name: "All", type: "select", "include-all": true, proxies: ["DIRECT"] }], rules: ["GEOIP,CN,DIRECT", "MATCH,DIRECT"] };
  const once = apply(input);
  const twice = apply(once);
  assert.deepEqual(twice, once);
  assert.deepEqual(apply(twice), once);
  assert.equal(reserved(twice).length, 5);
});

test("reserved name collisions or modified prior overlay fail without changing input", () => {
  const inputs = [
    { proxies: [{ name: "GWIFI-Wired-A", type: "socks5" }] },
    { "proxy-groups": [{ name: selector, type: "select", proxies: ["DIRECT"] }] },
    { proxies: [{ name: "GWIFI-Unrelated", type: "direct" }] },
    { "proxy-providers": { "GWIFI-Dual": { type: "file", path: "nodes.yaml" } } }
  ];
  const modified = apply({});
  modified.proxies[0]["interface-name"] = "Other";
  inputs.push(modified);
  const duplicated = apply({});
  duplicated.proxies.push(structuredClone(duplicated.proxies[0]));
  inputs.push(duplicated);
  for (const input of inputs) {
    const original = structuredClone(input);
    assert.throws(() => apply(input), /GWIFI: reserved/);
    assert.deepEqual(input, original);
  }
});

test("rejects malformed config containers rather than replacing them", () => {
  for (const input of [null, [], { proxies: {} }, { rules: "MATCH,DIRECT" }, { "proxy-groups": false }]) {
    assert.throws(() => apply(input), /GWIFI:/);
  }
  assert.throws(() => apply({ "proxy-groups": [{ name: "All", "include-all": true, "exclude-filter": [] }] }), /exclude-filter/);
});
