import { createHash } from "node:crypto";
import {
  existsSync,
  readFileSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const PATCH_MARKER = "TSUNAMI_ADMIN_PATCH_20260722";
const EXPECTED_SOURCE_SHA256 =
  "0cfb12e1ec9afee9e439744671de2c14cb1af1b3118c291f0a87348492948666";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(scriptDir, "..");
const adminRoot = join(projectRoot, "public", "assets", "admin");
const manifestPath = join(adminRoot, "manifest.json");
const indexPath = join(adminRoot, "index.html");
const checkOnly = process.argv.includes("--check");

function sha256(content) {
  return createHash("sha256").update(content).digest("hex");
}

function replaceOnce(source, needle, replacement, label) {
  const first = source.indexOf(needle);
  if (first === -1) {
    throw new Error(`Cannot find ${label} anchor`);
  }
  if (source.indexOf(needle, first + needle.length) !== -1) {
    throw new Error(`${label} anchor is not unique`);
  }
  return (
    source.slice(0, first) +
    replacement +
    source.slice(first + needle.length)
  );
}

function patchBundle(source) {
  let output = source;

  output = replaceOnce(
    output,
    'e.AnyTLS="anytls",e))(iqt||{})',
    'e.AnyTLS="anytls",e.Tsunami="tsunami",e))(iqt||{})',
    "protocol enum",
  );
  output = replaceOnce(
    output,
    '{type:"anytls",label:"AnyTLS"}],sqt=',
    '{type:"anytls",label:"AnyTLS"},{type:"tsunami",label:"TSUNAMI"}],sqt=',
    "protocol selector",
  );
  output = replaceOnce(
    output,
    'anytls:"#7E57C2"};var oqt=',
    'anytls:"#7E57C2",tsunami:"#00A6A6"};var oqt=',
    "protocol color",
  );

  const formImplementation = String.raw`,/* ${PATCH_MARKER} */
TsunamiSchema2026=()=>py({
  listen_ip:cy().default("0.0.0.0"),
  transport:py({
    mode:_y(["h2","websocket","raw"]).default("raw"),
    path:cy().default("/assets/update"),
    host:cy().default("")
  }).default({}),
  tls:py({
    server_name:cy().default(""),
    allow_insecure:uy().default(!1),
    cert_file:cy().default(""),
    key_file:cy().default("")
  }).default({}),
  fronting:py({
    enabled:uy().default(!1),
    secret:cy().default(""),
    server_header:cy().default("Caddy"),
    site_name:cy().default("Welcome"),
    decoy_proxy:cy().default("")
  }).default({}),
  surge:py({
    mode:_y(["auto","none"]).default("auto"),
    max_connections:yy().default(4),
    threshold:yy().default(8)
  }).default({}),
  security:py({
    device_limit_mode:_y(["ip","session"]).default("ip")
  }).default({}),
  routing:py({
    allow_all:uy().default(!1)
  }).default({}),
  fallback:cy().default(""),
  padding_scheme:cy().default("")
}),
TsunamiTextField2026=({form:e,name:t,label:n,placeholder:i="",type:r="text"})=>Q.jsx($y,{
  control:e.control,
  name:t,
  render:({field:e})=>Q.jsxs(Gy,{children:[
    Q.jsx(Zy,{className:"text-[12px] font-mono text-foreground/80",children:n}),
    Q.jsx(Yy,{children:Q.jsx(u8e,{
      className:"font-mono text-xs",
      type:r,
      placeholder:i,
      ...e,
      value:e.value??"",
      onChange:"number"===r?t=>{
        const n=t.target.value;
        e.onChange(""===n?void 0:parseInt(n,10))
      }:e.onChange
    })})
  ]})
}),
TsunamiTextareaField2026=({form:e,name:t,label:n,placeholder:i=""})=>Q.jsx($y,{
  control:e.control,
  name:t,
  render:({field:e})=>Q.jsxs(Gy,{children:[
    Q.jsx(Zy,{className:"text-[12px] font-mono text-foreground/80",children:n}),
    Q.jsx(Yy,{children:Q.jsx(aZt,{
      className:"min-h-[96px] resize-y font-mono text-xs",
      placeholder:i,
      ...e,
      value:e.value??""
    })})
  ]})
}),
TsunamiSelectField2026=({form:e,name:t,label:n,options:i})=>Q.jsx($y,{
  control:e.control,
  name:t,
  render:({field:e})=>Q.jsxs(Gy,{children:[
    Q.jsx(Zy,{className:"text-[12px] font-mono text-foreground/80",children:n}),
    Q.jsx(Yy,{children:Q.jsxs(yzt,{
      onValueChange:e.onChange,
      value:e.value,
      children:[
        Q.jsx(Czt,{className:"font-mono text-xs",children:Q.jsx(wzt,{placeholder:"请选择"})}),
        Q.jsx(Nzt,{hideWhenDetached:!0,children:Q.jsx(xzt,{children:i.map(e=>Q.jsx(Lzt,{
          value:e.value,
          children:e.label
        },e.value))})})
      ]
    })})
  ]})
}),
TsunamiSwitchField2026=({form:e,name:t,label:n})=>Q.jsx($y,{
  control:e.control,
  name:t,
  render:({field:e})=>Q.jsxs(Gy,{children:[
    Q.jsx(Zy,{className:"text-[12px] font-mono text-foreground/80",children:n}),
    Q.jsx("div",{className:"py-2",children:Q.jsx(Yy,{children:Q.jsx(oZt,{
      checked:!!e.value,
      onCheckedChange:e.onChange
    })})})
  ]})
}),
TsunamiSection2026=({title:e,children:t})=>Q.jsxs("section",{
  className:"space-y-3 border-t pt-4 first:border-t-0 first:pt-0",
  children:[
    Q.jsx("h3",{className:"text-xs font-semibold text-foreground",children:e}),
    t
  ]
}),
TsunamiForm2026=({form:e})=>Q.jsxs(Q.Fragment,{children:[
  Q.jsx(TsunamiSection2026,{title:"监听与传输",children:Q.jsxs("div",{
    className:"grid gap-3 md:grid-cols-2",
    children:[
      Q.jsx(TsunamiTextField2026,{form:e,name:"listen_ip",label:"监听地址",placeholder:"0.0.0.0"}),
      Q.jsx(TsunamiSelectField2026,{form:e,name:"transport.mode",label:"传输模式",options:[
        {value:"raw",label:"原始传输 (raw)"},
        {value:"h2",label:"HTTP/2 (h2)"},
        {value:"websocket",label:"WebSocket"}
      ]}),
      Q.jsx(TsunamiTextField2026,{form:e,name:"transport.path",label:"传输路径",placeholder:"/assets/update"}),
      Q.jsx(TsunamiTextField2026,{form:e,name:"transport.host",label:"Host 覆盖",placeholder:"cdn.example.com"})
    ]
  })}),
  Q.jsx(TsunamiSection2026,{title:"TLS",children:Q.jsxs("div",{
    className:"grid gap-3 md:grid-cols-2",
    children:[
      Q.jsx(TsunamiTextField2026,{form:e,name:"tls.server_name",label:"TLS 服务名 (SNI)",placeholder:"node.example.com"}),
      Q.jsx(TsunamiSwitchField2026,{form:e,name:"tls.allow_insecure",label:"允许不安全证书"}),
      Q.jsx(TsunamiTextField2026,{form:e,name:"tls.cert_file",label:"证书文件",placeholder:"/etc/tsunami/cert.pem"}),
      Q.jsx(TsunamiTextField2026,{form:e,name:"tls.key_file",label:"私钥文件",placeholder:"/etc/tsunami/key.pem"})
    ]
  })}),
  Q.jsx(TsunamiSection2026,{title:"前置伪装",children:Q.jsxs("div",{
    className:"grid gap-3 md:grid-cols-2",
    children:[
      Q.jsx(TsunamiSwitchField2026,{form:e,name:"fronting.enabled",label:"启用前置伪装"}),
      e.watch("fronting.enabled")&&Q.jsx(TsunamiTextField2026,{form:e,name:"fronting.secret",label:"前置密钥"}),
      e.watch("fronting.enabled")&&Q.jsx(TsunamiTextField2026,{form:e,name:"fronting.server_header",label:"Server 响应头",placeholder:"Caddy"}),
      e.watch("fronting.enabled")&&Q.jsx(TsunamiTextField2026,{form:e,name:"fronting.site_name",label:"站点名称",placeholder:"Welcome"}),
      e.watch("fronting.enabled")&&Q.jsx(TsunamiTextField2026,{form:e,name:"fronting.decoy_proxy",label:"诱饵代理",placeholder:"http://127.0.0.1:8080"})
    ]
  })}),
  Q.jsx(TsunamiSection2026,{title:"突发与安全",children:Q.jsxs("div",{
    className:"grid gap-3 md:grid-cols-2",
    children:[
      Q.jsx(TsunamiSelectField2026,{form:e,name:"surge.mode",label:"突发模式",options:[
        {value:"auto",label:"自动"},
        {value:"none",label:"关闭"}
      ]}),
      "auto"===e.watch("surge.mode")&&Q.jsx(TsunamiTextField2026,{form:e,name:"surge.max_connections",label:"最大连接数",type:"number",placeholder:"4"}),
      "auto"===e.watch("surge.mode")&&Q.jsx(TsunamiTextField2026,{form:e,name:"surge.threshold",label:"触发阈值",type:"number",placeholder:"8"}),
      Q.jsx(TsunamiSelectField2026,{form:e,name:"security.device_limit_mode",label:"设备限制模式",options:[
        {value:"ip",label:"宽松：同一 IP 计为一个设备"},
        {value:"session",label:"严格：每个连接会话计为设备"}
      ]}),
      Q.jsx(TsunamiSwitchField2026,{form:e,name:"routing.allow_all",label:"允许全部路由"})
    ]
  })}),
  Q.jsx(TsunamiSection2026,{title:"其他",children:Q.jsxs("div",{
    className:"grid gap-3 md:grid-cols-2",
    children:[
      Q.jsx(TsunamiTextField2026,{form:e,name:"fallback",label:"回落目标",placeholder:"127.0.0.1:8080"}),
      Q.jsx(TsunamiTextareaField2026,{form:e,name:"padding_scheme",label:"填充方案"})
    ]
  })})
]}),
s5t=((e,t)=>({`;

  output = replaceOnce(
    output,
    ",s5t=((e,t)=>({",
    formImplementation,
    "dynamic form registry",
  );
  output = replaceOnce(
    output,
    "mieru:{schema:n5t(t),component:r5t}}))",
    "mieru:{schema:n5t(t),component:r5t},tsunami:{schema:TsunamiSchema2026(),component:TsunamiForm2026}}))",
    "TSUNAMI form registration",
  );
  output = replaceOnce(
    output,
    "{value:iqt.Mieru,label:rqt.find(e=>e.type===iqt.Mieru)?.label,color:sqt[iqt.Mieru]}];function x5t",
    "{value:iqt.Mieru,label:rqt.find(e=>e.type===iqt.Mieru)?.label,color:sqt[iqt.Mieru]},{value:iqt.Tsunami,label:rqt.find(e=>e.type===iqt.Tsunami)?.label,color:sqt[iqt.Tsunami]}];function x5t",
    "node type filter",
  );

  return output;
}

function verifyPatched(source) {
  const requiredTokens = [
    PATCH_MARKER,
    'e.Tsunami="tsunami"',
    '{type:"tsunami",label:"TSUNAMI"}',
    'tsunami:"#00A6A6"',
    "security.device_limit_mode",
    "宽松：同一 IP 计为一个设备",
    "严格：每个连接会话计为设备",
    "tsunami:{schema:TsunamiSchema2026(),component:TsunamiForm2026}",
    "value:iqt.Tsunami",
  ];

  for (const token of requiredTokens) {
    if (!source.includes(token)) {
      throw new Error(`Patched bundle is missing token: ${token}`);
    }
  }
}

if (!existsSync(manifestPath) || !existsSync(indexPath)) {
  throw new Error(`XBoard admin assets not found at ${adminRoot}`);
}

const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
const entry = manifest["index.html"];
if (!entry?.file) {
  throw new Error("manifest.json does not contain the index.html entry");
}

const sourceEntry = entry.file.replaceAll("/", "\\");
const sourcePath = join(adminRoot, sourceEntry);
if (!existsSync(sourcePath)) {
  throw new Error(`Admin entry bundle does not exist: ${sourcePath}`);
}

let bundle = readFileSync(sourcePath, "utf8");
if (!bundle.includes(PATCH_MARKER)) {
  const sourceHash = sha256(bundle);
  if (sourceHash !== EXPECTED_SOURCE_SHA256) {
    throw new Error(
      `Unsupported admin bundle SHA256 ${sourceHash}; expected ${EXPECTED_SOURCE_SHA256}`,
    );
  }
  if (checkOnly) {
    throw new Error("TSUNAMI admin patch has not been applied");
  }
  bundle = patchBundle(bundle);
}

verifyPatched(bundle);

const patchedHash = sha256(bundle).slice(0, 8);
const patchedEntry = `assets/index-tsunami-${patchedHash}.js`;
const patchedPath = join(adminRoot, ...patchedEntry.split("/"));

if (checkOnly) {
  if (entry.file !== patchedEntry) {
    throw new Error(
      `manifest entry is ${entry.file}, expected ${patchedEntry}`,
    );
  }
  const indexHtml = readFileSync(indexPath, "utf8");
  if (!indexHtml.includes(`./${patchedEntry}`)) {
    throw new Error(`index.html does not reference ./${patchedEntry}`);
  }
  console.log(`TSUNAMI admin patch verified: ${patchedEntry}`);
  process.exit(0);
}

if (sourcePath !== patchedPath) {
  const temporaryPath = `${patchedPath}.tmp`;
  writeFileSync(temporaryPath, bundle);
  renameSync(temporaryPath, patchedPath);
}

let indexHtml = readFileSync(indexPath, "utf8");
const oldScript = `./${entry.file}`;
const newScript = `./${patchedEntry}`;
if (oldScript !== newScript) {
  indexHtml = replaceOnce(
    indexHtml,
    oldScript,
    newScript,
    "index.html entry script",
  );
  writeFileSync(indexPath, indexHtml);
}

entry.file = patchedEntry;
writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);

if (sourcePath !== patchedPath && existsSync(sourcePath)) {
  unlinkSync(sourcePath);
}

console.log(`TSUNAMI admin patch applied: ${patchedEntry}`);
console.log(`SHA256: ${sha256(bundle)}`);
