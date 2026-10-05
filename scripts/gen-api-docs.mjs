#!/usr/bin/env node
/**
 * scripts/gen-api-docs.mjs — 接口文档 / Postman / RPC 冒烟脚本生成器
 *
 * 真源只有三处：
 *   1. gateway/{app,admin}/api/*.api        —— 对外 HTTP 契约
 *   2. services/&lt;svc&gt;/rpc/&lt;name&gt;.proto       —— 内部 RPC 契约
 *   3. gateway/admin/internal/middleware/*  —— routePermissions 权限表
 *
 * 本脚本不手工罗列任何路由或方法；生成前先校验「.api 声明的路由集合」与
 * goctl 生成的 internal/handler/routes.go 完全一致，不一致直接失败退出，
 * 避免文档悄悄落后于契约（AGENTS.md §4 生成一致性）。
 *
 * 用法：
 *   node scripts/gen-api-docs.mjs           生成并写盘
 *   node scripts/gen-api-docs.mjs --check   只校验：漂移门禁 + 磁盘产物是否已是最新
 */

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const CHECK = process.argv.includes('--check')
const GENERATED_NOTE =
  '由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。'
const MISSING = '（**契约中找不到该类型，请核对 .api**）'

// ---------------------------------------------------------------- 基础工具

const read = (rel) => fs.readFileSync(path.join(ROOT, rel), 'utf8').replace(/\r\n/g, '\n')
const exists = (rel) => fs.existsSync(path.join(ROOT, rel))

function glob(dir, pred, out = []) {
  for (const ent of fs.readdirSync(path.join(ROOT, dir), { withFileTypes: true })) {
    const rel = path.posix.join(dir, ent.name)
    if (ent.isDirectory()) glob(rel, pred, out)
    else if (pred(rel)) out.push(rel)
  }
  return out.sort()
}

/** Markdown 表格单元格转义：竖线和换行会破坏表格。 */
const cell = (s) => String(s ?? '').replace(/\|/g, '\\|').replace(/\n+/g, ' / ').trim() || '—'

/** 去重保序。 */
function uniq(arr) {
  const seen = new Set()
  return arr.filter((x) => (seen.has(x) ? false : seen.add(x)))
}

// ---------------------------------------------------------------- .api 解析

function parseTag(tag) {
  if (!tag) return { location: '', key: '', opts: [] }
  const m = tag.match(/^(\w+):"([^"]*)"$/)
  if (!m) return { location: '', key: tag, opts: [] }
  const parts = m[2].split(',')
  return { location: m[1], key: parts[0], opts: parts.slice(1).filter(Boolean) }
}

/** 剥掉行尾 `//` 注释（标签反引号内的 `//` 不算注释）。 */
function stripTrailingComment(line) {
  const idx = line.lastIndexOf('//')
  if (idx <= 0) return { code: line, comment: '' }
  const tagMatch = line.match(/`[^`]*`/)
  const tagEnd = tagMatch ? line.indexOf(tagMatch[0]) + tagMatch[0].length : 0
  if (idx < tagEnd) return { code: line, comment: '' }
  return { code: line.slice(0, idx).trim(), comment: line.slice(idx + 2).trim() }
}

/**
 * 解析 goctl 方言的 .api：type 块 + @server(prefix/middleware) + service 块路由。
 * 任何解析不了的行都抛错，不允许静默丢接口。
 */
function parseApi(rel) {
  const lines = read(rel).split('\n')
  const types = new Map()
  const sections = []
  let title = ''
  let notes = []
  let i = 0
  const strip = (s) => s.replace(/^\s*\/\/\s?/, '')

  while (i < lines.length) {
    const line = lines[i].trim()
    if (line === '') { i++; continue }

    const hm = line.match(/^\/\/\s*={3,}\s*(.*)$/)
    if (hm) {
      title = hm[1].replace(/=+\s*$/, '').trim()
      notes = []
      i++
      continue
    }
    if (line.startsWith('//')) { notes.push(strip(line)); i++; continue }

    let m = line.match(/^type\s+(\w+)\s*\{\s*$/)
    if (m || /^type\s+\w+\s*\{\s*\}$/.test(line)) {
      if (!m) {
        const em = line.match(/^type\s+(\w+)\s*\{\s*\}$/)
        types.set(em[1], { name: em[1], fields: [], note: notes.join(' / '), title })
        notes = []
        i++
        continue
      }
      const name = m[1]
      const fields = []
      let fieldNotes = []
      i++
      while (i < lines.length && lines[i].trim() !== '}') {
        const fl = lines[i].trim()
        if (fl === '') { i++; continue }
        if (fl.startsWith('//')) { fieldNotes.push(strip(fl)); i++; continue }
        const { code, comment } = stripTrailingComment(fl)
        const fm = code.match(/^(\w+)\s+(\S+)\s*(?:`([^`]*)`)?\s*$/)
        if (!fm) throw new Error(`${rel}:${i + 1} 无法解析 type ${name} 的字段行：${fl}`)
        fields.push({
          name: fm[1],
          goType: fm[2],
          tag: fm[3] || '',
          note: [...fieldNotes, comment].filter(Boolean).join(' / '),
        })
        fieldNotes = []
        i++
      }
      types.set(name, { name, fields, note: notes.join(' / '), title })
      notes = []
      i++
      continue
    }

    m = line.match(/^@server\s*\(\s*$/)
    if (m) {
      let prefix = ''
      let middleware = ''
      i++
      while (i < lines.length && lines[i].trim() !== ')') {
        const sl = lines[i].trim()
        const pm = sl.match(/^prefix:\s*(\S+)/)
        if (pm) prefix = pm[1]
        const mm = sl.match(/^middleware:\s*(\S+)/)
        if (mm) middleware = mm[1]
        i++
      }
      i++
      const svc = lines[i]?.trim().match(/^service\s+(\w+)\s*\{\s*$/)
      if (!svc) throw new Error(`${rel}:${i + 1} @server 之后缺少 service 块`)
      i++
      const routes = []
      let doc = ''
      let handler = ''
      while (i < lines.length && lines[i].trim() !== '}') {
        const rl = stripTrailingComment(lines[i].trim()).code
        if (rl === '' || rl.startsWith('//')) { i++; continue }
        const dm = rl.match(/^@doc\s+"(.*)"$/)
        if (dm) { doc = dm[1]; i++; continue }
        const hm2 = rl.match(/^@handler\s+(\w+)$/)
        if (hm2) { handler = hm2[1]; i++; continue }
        const rm = rl.match(
          /^(get|post|put|delete|head)\s+(\S+)(?:\s*\(\s*([\w.[\]]+)\s*\))?\s*(?:returns\s*\(\s*([\w.[\]]+)\s*\))?\s*$/,
        )
        if (rm) {
          if (!handler) throw new Error(`${rel}:${i + 1} 路由缺少 @handler：${rl}`)
          routes.push({
            method: rm[1].toUpperCase(),
            path: rm[2],
            reqType: rm[3] || '',
            respType: rm[4] || '',
            doc,
            handler,
          })
          doc = ''
          handler = ''
          i++
          continue
        }
        throw new Error(`${rel}:${i + 1} 无法解析路由行：${rl}`)
      }
      sections.push({ prefix, middleware, title, notes: notes.slice(), routes })
      notes = []
      i++
      continue
    }
    i++
  }
  return { types, sections }
}

/** 一个类型里引用到的其它已定义类型名。 */
function typeRefs(type, types) {
  const out = []
  for (const f of type.fields) {
    for (const id of f.goType.match(/[A-Za-z_]\w*/g) || []) if (types.has(id)) out.push(id)
  }
  return uniq(out)
}

/** 从若干根类型出发求传递闭包（含根）。 */
function reachable(roots, types) {
  const seen = new Set()
  const queue = roots.filter(Boolean).filter((r) => types.has(r))
  while (queue.length) {
    const n = queue.shift()
    if (seen.has(n)) continue
    seen.add(n)
    queue.push(...typeRefs(types.get(n), types))
  }
  return [...seen].map((n) => types.get(n))
}

// ---------------------------------------------------------------- routes.go 解析（漂移门禁用）

/** 从 goctl 生成的 routes.go 抽出注册后的完整路径与方法。 */
function parseRoutesGo(rel) {
  const lines = read(rel).split('\n')
  const routes = []
  let pending = []
  let prefix = ''
  let middleware = null
  let method = ''
  for (const raw of lines) {
    const l = raw.trim()
    if (l.startsWith('server.AddRoutes(')) { pending = []; prefix = ''; middleware = null; continue }
    let m = l.match(/^Method:\s*http\.Method(\w+),$/)
    if (m) { method = m[1].toUpperCase(); continue }
    m = l.match(/^Path:\s*"([^"]+)",$/)
    if (m) { pending.push({ method, path: m[1] }); continue }
    m = l.match(/^rest\.WithPrefix\("([^"]+)"\),?$/)
    if (m) { prefix = m[1]; continue }
    m = l.match(/^\[\]rest\.Middleware\{serverCtx\.(\w+)\},$/)
    if (m) { middleware = m[1]; continue }
    if (l === ')') {
      for (const r of pending) routes.push({ method: r.method, path: prefix + r.path, middleware })
      pending = []
    }
  }
  return routes
}

const keyOf = (m, p) => `${m} ${p}`

// ---------------------------------------------------------------- proto 解析

function parseProto(rel) {
  const lines = read(rel).split('\n')
  const out = { file: rel, package: '', goPackage: '', header: [], types: [], services: [] }
  const stack = []          // { kind: 'message'|'enum'|'service', name, container }
  let notes = []
  let declared = false      // 是否已进入第一条声明
  const headerBlocks = []   // 声明之前的注释块，按空行分块（最后一块属于首条声明）

  const qualified = (name) => {
    const parents = stack.filter((s) => s.kind === 'message').map((s) => s.name)
    return [...parents, name].join('.')
  }

  const takeHeader = () => {
    const blocks = notes.length ? [...headerBlocks, notes.join('\n')] : headerBlocks.slice()
    const last = blocks.length ? blocks[blocks.length - 1] : ''
    out.header = blocks.slice(0, -1).join('\n \n').split('\n')
    notes = last ? last.split('\n') : []
    declared = true
  }

  for (const raw of lines) {
    const l = raw.trim()
    if (l === '') {
      if (!declared && notes.length) { headerBlocks.push(notes.join('\n')); notes = [] }
      continue
    }
    if (l.startsWith('//')) { notes.push(l.replace(/^\/\/\s?/, '')); continue }

    let m = l.match(/^package\s+(\S+);$/)
    if (m) { out.package = m[1]; notes = []; continue }
    m = l.match(/^option\s+go_package\s*=\s*"([^"]+)";/)
    if (m) { out.goPackage = m[1]; notes = []; continue }
    if (/^(syntax|option|import)\b/.test(l)) { notes = []; continue }

    m = l.match(/^(message|enum)\s+(\w+)\s*\{\s*\}$/)
    if (m) {
      if (!declared) takeHeader()
      out.types.push({
        kind: m[1],
        name: qualified(m[2]),
        simpleName: m[2],
        comment: notes.join(' / '),
        fields: [],
        values: [],
      })
      notes = []
      continue
    }
    m = l.match(/^(message|enum)\s+(\w+)\s*\{$/)
    if (m) {
      if (!declared) takeHeader()
      const t = {
        kind: m[1],
        name: qualified(m[2]),
        simpleName: m[2],
        comment: notes.join(' / '),
        fields: [],
        values: [],
      }
      out.types.push(t)
      stack.push({ kind: m[1], name: m[2], container: t })
      notes = []
      continue
    }
    m = l.match(/^service\s+(\w+)\s*\{$/)
    if (m) {
      if (!declared) takeHeader()
      const s = { name: m[1], comment: notes.join(' / '), methods: [] }
      out.services.push(s)
      stack.push({ kind: 'service', name: m[1], container: s })
      notes = []
      continue
    }
    if (l === '}') { stack.pop(); notes = []; continue }

    const top = stack[stack.length - 1]
    if (!top) { notes = []; continue }

    if (top.kind === 'message') {
      m = l.match(/^(?:(repeated|optional)\s+)?(.+?)\s+(\w+)\s*=\s*(\d+)\s*(\[[^\]]*\])?;(.*)$/)
      if (m) {
        const comment = (m[6].match(/\/\/(.*)$/) || [, ''])[1].trim()
        top.container.fields.push({
          label: m[1] || '',
          type: m[2].trim(),
          name: m[3],
          num: Number(m[4]),
          opts: m[5] || '',
          comment: [notes.join(' / '), comment].filter(Boolean).join(' / '),
        })
        notes = []
        continue
      }
      if (/^\s*(reserved|option|;)/.test(l)) { notes = []; continue }
      throw new Error(`${rel} 无法解析 message 行：${l}`)
    }
    if (top.kind === 'enum') {
      m = l.match(/^(\w+)\s*=\s*(-?\d+)\s*(\[[^\]]*\])?;(.*)$/)
      if (m) {
        top.container.values.push({
          name: m[1],
          num: Number(m[2]),
          comment: (m[4].match(/\/\/(.*)$/) || [, ''])[1].trim(),
        })
        notes = []
        continue
      }
      if (/^\s*(reserved|option)/.test(l)) { notes = []; continue }
      throw new Error(`${rel} 无法解析 enum 行：${l}`)
    }
    if (top.kind === 'service') {
      m = l.match(/^rpc\s+(\w+)\s*\(\s*(stream\s+)?(\w+)\s*\)\s*returns\s*\(\s*(stream\s+)?(\w+)\s*\)\s*(\{|;)(.*)$/)
      if (m) {
        top.container.methods.push({
          name: m[1],
          clientStream: Boolean(m[2]),
          serverStream: Boolean(m[4]),
          in: m[3],
          out: m[5],
          comment: notes.join(' / ') || (m[7].match(/\/\/(.*)$/) || [, ''])[1].trim(),
        })
        notes = []
        continue
      }
      if (l === '{') { top.inBody = true; notes = []; continue }
      if (top.inBody) { if (l === '}') top.inBody = false; continue }
      if (l === ';') continue
      throw new Error(`${rel} 无法解析 service 行：${l}`)
    }
    notes = []
  }
  return out
}

// ---------------------------------------------------------------- etc / 网关配置解析

/** 读 services/<dir>/etc/*.yaml 的 Name 与 ListenOn（zrpc 服务名即 etcd key）。 */
function readServiceConf(svcDir) {
  const dir = path.join(ROOT, 'services', svcDir, 'etc')
  if (!fs.existsSync(dir)) return { name: '', port: '', dsn: '', file: '' }
  const file = fs.readdirSync(dir).filter((f) => f.endsWith('.yaml')).sort()[0]
  const src = fs.readFileSync(path.join(dir, file), 'utf8').replace(/\r\n/g, '\n').split('\n')
  const conf = { name: '', regKey: '', port: '', dsn: '', file: `services/${svcDir}/etc/${file}` }
  let inTopEtcd = false
  for (const raw of src) {
    const l = raw.trim()
    let m = l.match(/^Name:\s*(\S+)/)
    if (m && !conf.name) conf.name = m[1]
    // 顶层 Etcd 块的 Key 才是 zrpc 注册/发现用的 key；Name 只是配置里的服务名，两者可以不同
    if (/^Etcd:\s*$/.test(raw)) { inTopEtcd = true; continue }
    if (inTopEtcd) {
      m = raw.match(/^\s+Key:\s*(\S+)/)
      if (m && !conf.regKey) { conf.regKey = m[1]; inTopEtcd = false; continue }
      if (/^\S/.test(raw)) inTopEtcd = false
    }
    m = l.match(/^ListenOn:\s*(\S+)/)
    if (m) conf.port = m[1].split(':').pop()
    m = l.match(/^DataSource:\s*(\S+)/)
    if (m && !conf.dsn) {
      const db = m[1].match(/\/([\w-]+)\?/)
      conf.dsn = db ? db[1] : ''
    }
  }
  if (!conf.regKey) conf.regKey = conf.name
  return conf
}

/** 网关 yaml 里 `XxxRPC: { Etcd: { Key: k } }` 的字段名 → etcd key。 */
function readGatewayRpcKeys(gwRel) {
  const out = new Map()
  if (!exists(gwRel)) return out
  let field = ''
  for (const raw of read(gwRel).split('\n')) {
    const m0 = raw.match(/^([A-Za-z]\w*RPC):\s*$/)
    if (m0) { field = m0[1]; continue }
    const m1 = raw.match(/^\s+Key:\s*(\S+)/)
    if (m1 && field) { out.set(m1[1], field); field = '' }
  }
  return out
}

/** 读 routePermissions：完整路径 → { Resource, Action }。 */
function readRoutePermissions(rel) {
  const out = new Map()
  for (const raw of read(rel).split('\n')) {
    const m = raw.match(/^\s*"([^"]+)":\s*\{Resource:\s*"([^"]+)",\s*Action:\s*"([^"]+)"\}/)
    if (m) out.set(m[1], { resource: m[2], action: m[3] })
  }
  return out
}

// ---------------------------------------------------------------- 网关元数据

const GATEWAYS = [
  {
    name: 'app',
    api: 'gateway/app/api/app.api',
    routes: 'gateway/app/internal/handler/routes.go',
    logicDir: 'gateway/app/internal/logic',
    etc: 'gateway/app/etc/app.yaml',
    port: '8080',
    baseVar: '{{appUrl}}',
  },
  {
    name: 'admin',
    api: 'gateway/admin/api/admin.api',
    routes: 'gateway/admin/internal/handler/routes.go',
    logicDir: 'gateway/admin/internal/logic',
    etc: 'gateway/admin/etc/admin.yaml',
    port: '8081',
    baseVar: '{{adminUrl}}',
  },
]

const AUTH_LABEL = {
  '': {
    app: '免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）',
    admin: '免鉴权（刻意不进 `routePermissions` 的只读运营面）',
  },
  AppkeyVerify: { app: '`AppkeyVerify`：query 参数 `appkey` 必须在白名单内，否则 403' },
  AdminPermission: { admin: '`AdminPermission`：需 Bearer 会话 + 下表权限点' },
}

function authOf(gw, middleware, fullPath, perms) {
  if (middleware === 'AdminPermission') {
    const p = perms.get(fullPath)
    return p
      ? `AdminPermission · 权限点 \`${p.resource}\` / \`${p.action}\``
      : 'AdminPermission · **权限表未登记 → 中间件 fail-closed 一律拒绝**'
  }
  if (middleware === 'AppkeyVerify') return 'AppkeyVerify · query `appkey` 白名单'
  return '免鉴权'
}

// ---------------------------------------------------------------- HTTP 文档渲染

const logicFileOf = (handler) => `${handler.toLowerCase()}logic.go`

function fieldTable(fields, kinds) {
  if (!fields.length) return '（该类型无字段：空请求 / 空响应。）'
  const rows = fields.map((f) => {
    const t = parseTag(f.tag)
    const optional = t.opts.includes('optional')
    const constraints = t.opts.filter((o) => o !== 'optional').join(', ')
    return `| \`${cell(f.name)}\` | \`${cell(t.key)}\` | ${t.location || '—'} | \`${cell(f.goType)}\` | ${
      optional ? '否' : '是'
    } | ${cell(constraints)} | ${cell(f.note)} |`
  })
  return [
    `| Go 字段 | ${kinds} | 位置 | 类型 | 必填 | 默认/约束 | 说明 |`,
    '|---|---|---|---|---|---|---|',
    ...rows,
  ].join('\n')
}

function typeAppendix(list) {
  if (!list.length) return ''
  const parts = ['## 类型附录', '']
  for (const t of list) {
    parts.push(`### \`${t.name}\``)
    if (t.note) parts.push(``, `> ${t.note}`)
    parts.push('', fieldTable(t.fields, '参数/JSON 键'), '')
  }
  return parts.join('\n')
}

/** 把一个网关的 sections 按前缀聚成域分组文件。 */
function buildHttpGroups(gw, perms) {
  const groups = []
  const index = new Map()
  for (const s of gw.sections) {
    if (!index.has(s.prefix)) {
      const g = { prefix: s.prefix, sections: [], routes: 0, auths: new Set() }
      index.set(s.prefix, g)
      groups.push(g)
    }
    const g = index.get(s.prefix)
    for (const r of s.routes) {
      g.routes++
      g.auths.add(r.middleware || '免鉴权')
    }
    g.sections.push(s)
  }
  return groups
}

/** .api 里通用小节标题（如「路由」）不带域信息，退回用组注释当标题。 */
const GENERIC_TITLES = new Set(['', '路由', '请求参数', '通用载荷', '响应数据载荷'])
function sectionHead(s, prefix) {
  if (s.title && !GENERIC_TITLES.has(s.title)) return s.title
  const fromNote = (s.notes[0] || '').replace(/（.*$/, '').trim()
  if (fromNote && !GENERIC_TITLES.has(fromNote)) return fromNote.length > 48 ? fromNote.slice(0, 48) + '…' : fromNote
  return `${prefix} 路由组`
}

function renderHttpGroupFile(gwName, gw, group, types, perms, num) {
  const slug = prefixSlug(group.prefix)
  const L = []
  L.push(`# ${gwName === 'app' ? '终端面' : '运营面'} · \`${group.prefix}\``, '')
  L.push(`> ${GENERATED_NOTE}`, '')
  L.push(
    `> 真源：\`gateway/${gwName}/api/${gwName}.api\`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。`, '',
  )

  const routes = group.sections.flatMap((s) => s.routes.map((r) => ({ ...r, middleware: s.middleware })))
  L.push(
    `## 本组概览`, '',
    `| 小节 | 鉴权 | 路由数 |`,
    `|---|---|---|`,
    ...group.sections.map((s) => {
      return `| ${cell(sectionHead(s, group.prefix))} | ${cell(s.middleware || '免鉴权')} | ${s.routes.length} |`
    }),
    '',
    `合计 **${routes.length}** 条。`, '',
    `入参编码看下方各表的「位置」列：\`path\`→路径段、\`form\`→URL 查询串（POST 也一样）、\`json\`→JSON 请求体。`,
    `为什么 \`form\` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。`, '',
  )

  for (const s of group.sections) {
    const head = sectionHead(s, group.prefix)
    L.push(`## ${head}（${s.middleware || '免鉴权'}，${s.routes.length} 条）`, '')
    const extra = s.notes.filter((n) => n !== head)
    if (extra.length) L.push(...extra.map((n) => `> ${n}`), '')
    const isProtected = s.middleware === 'AdminPermission'
    L.push(
      isProtected
        ? `鉴权：\`AdminPermission\` —— 需 \`Authorization: Bearer <admin_token>\`，再按下表「权限点」判定；` +
          `中间件对 \`routePermissions\` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。`
        : `鉴权：${(AUTH_LABEL[s.middleware] || {})[gwName] || s.middleware || '免鉴权'}`,
      '',
      isProtected
        ? `| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |`
        : `| 方法 | 完整路径 | 说明 | handler | logic 文件 |`,
      isProtected ? `|---|---|---|---|---|---|` : `|---|---|---|---|---|`,
    )
    for (const r of s.routes) {
      const full = group.prefix + r.path
      const p = perms.get(full)
      const permCell = isProtected
        ? (p ? `\`${p.resource}\` / \`${p.action}\`` : '**未登记 → 拒绝**') + ' | '
        : ''
      L.push(
        `| ${r.method} | \`${full}\` | ${cell(r.doc)} | ${permCell}\`${r.handler}\` | \`${logicFileOf(r.handler)}\` |`,
      )
    }
    L.push('')

    for (const r of s.routes) {
      const full = group.prefix + r.path
      L.push(`### ${r.method} \`${full}\` — ${r.doc || '（契约未写 @doc）'}`, '')
      L.push(
        `- 权限口径：${authOf(gwName, s.middleware, full, perms)}`,
        `- goctl 入口：\`gateway/${gwName}/internal/handler/${r.handler.toLowerCase()}handler.go\``,
        `- 业务实现：\`gateway/${gwName}/internal/logic/${logicFileOf(r.handler)}\``,
      )
      if (r.reqType) {
        const t = types.get(r.reqType)
        L.push('', `请求：\`${r.reqType}\`` + (t ? '' : MISSING), '')
        if (t) {
          if (r.method === 'GET' || r.method === 'DELETE' || r.method === 'HEAD') {
            const q = t.fields.filter((f) => parseTag(f.tag).location !== 'path')
            L.push(fieldTable(q, 'query 参数'), '')
            const pp = t.fields.filter((f) => parseTag(f.tag).location === 'path')
            if (pp.length) L.push('路径参数：', '', fieldTable(pp, '路径段'), '')
          } else {
            L.push(fieldTable(t.fields, 'JSON 字段'), '')
          }
        }
      } else {
        L.push('', '请求：无参数体。', '')
      }
      if (r.respType) {
        const t = types.get(r.respType)
        L.push(`响应：\`${r.respType}\`` + (t ? '' : MISSING), '')
        if (t) {
          L.push(fieldTable(t.fields, 'JSON 字段'), '')
          L.push('', '响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。', '')
        }
      } else {
        L.push('', '响应：无返回体（生成物返回空 data）。', '')
      }
    }
  }

  const roots = routes.flatMap((r) => [r.reqType, r.respType])
  L.push(typeAppendix(reachable(roots, types)))
  L.push(``, `<!-- file: docs/api/http/${gwName}/${num}-${slug}.md -->`)
  return L.join('\n')
}

function prefixSlug(prefix) {
  const s = prefix.replace(/^\//, '').replace(/\//g, '-').replace(/^-+|-+$/g, '')
  return s || 'root'
}

// ---------------------------------------------------------------- Postman 生成

const QUERY_METHODS = new Set(['GET', 'HEAD', 'DELETE'])

const NAME_VAR = {
  mid: '{{mid}}', uid: '{{mid}}', user_id: '{{mid}}',
  aid: '{{aid}}', avid: '{{aid}}', bvid: '{{bvid}}', bv_id: '{{bvid}}',
  ip: '{{clientIp}}', client_ip: '{{clientIp}}', appkey: '{{appkey}}', app_key: '{{appkey}}',
  biz_no: '{{$uuid}}', idempotency_key: '{{$uuid}}', request_id: '{{$uuid}}', req_id: '{{$uuid}}',
  nonce: '{{$uuid}}', trace_id: '{{$guid}}', out_trade_no: '{{$uuid}}',
  keyword: 'keyword', query: 'keyword', q: 'keyword',
}
const NUM_DEFAULT = { pn: 1, page: 1, page_no: 1, cursor: 1, ps: 20, page_size: 20, limit: 20, size: 20 }
const ENUMISH = { typeid: 1, type: 1, plat: 1, platform: 1, mode: 1, state: 1, status: 1, role: 1, action: 1 }

const isNumeric = (gt) => /^(u?int|float|double)/.test(gt)
const elemOf = (gt) => (gt.startsWith('[]') ? gt.slice(2) : gt)

/**
 * go-zero 的 httpx.Parse 对每个请求都会依次做 ParsePath → ParseForm → ParseHeaders → ParseJsonBody，
 * 其中 ParseForm 取的是 r.Form（URL 查询串 + urlencoded 体），所以 `form` 标签一律落到查询参数最稳，
 * `json` 标签（和无标签的裸字段）落到 JSON 体。集合按这个口径生成，客户端只需要换一种编码方式即可复用。
 */
function splitByLocation(fields) {
  const out = { path: [], query: [], body: [] }
  for (const f of fields) {
    const loc = parseTag(f.tag).location
    if (loc === 'path') out.path.push(f)
    else if (loc === 'form') out.query.push(f)
    else out.body.push(f)
  }
  return out
}

/** 查询参数值：一律字符串形态；go-zero 支持逗号数组，故 []T 只给一个元素。 */
function queryValue(f, types) {
  const name = parseTag(f.tag).key || f.name
  if (NAME_VAR[name]) return NAME_VAR[name]
  const gt = elemOf(f.goType)
  if (gt.startsWith('map[')) {
    throw new Error(`查询参数 ${name} 的类型 ${f.goType} 是 map，无法编码进 URL，请修正 .api`)
  }
  if (types.has(gt)) {
    throw new Error(`查询参数 ${name} 的类型 ${f.goType} 是结构体，无法编码进 URL，请改用 json 标签走请求体`)
  }
  if (isNumeric(gt)) return String(NUM_DEFAULT[name] ?? ENUMISH[name] ?? 1)
  if (gt === 'bool') return 'false'
  return ''
}

/** JSON body 叶子值；{ raw } 表示要输出成不带引号的 JSON 字面量（变量/数字）。 */
function jsonLeaf(f, types) {
  const name = parseTag(f.tag).key || f.name
  const gt = f.goType
  if (isNumeric(gt)) {
    if (NAME_VAR[name]) return { raw: NAME_VAR[name] }
    return NUM_DEFAULT[name] ?? ENUMISH[name] ?? 0
  }
  if (gt === 'bool') return false
  if (NAME_VAR[name]) return NAME_VAR[name]
  return ''
}

function objectFromFields(fields, types, depth = 0, seen = new Set()) {
  const obj = {}
  for (const f of fields) {
    const tag = parseTag(f.tag)
    const key = tag.key || f.name
    const gt = f.goType
    if (gt.startsWith('[]')) {
      const inner = { ...f, goType: elemOf(gt), tag: '' }
      const el = inner.goType
      obj[key] = [types.has(el) ? bodyObject(el, types, depth + 1, seen) : jsonLeaf(inner, types)]
      continue
    }
    if (gt.startsWith('map[')) { obj[key] = {}; continue }
    if (types.has(gt)) { obj[key] = bodyObject(gt, types, depth + 1, seen); continue }
    obj[key] = jsonLeaf(f, types)
  }
  return obj
}

function bodyObject(typeName, types, depth = 0, seen = new Set()) {
  const t = types.get(typeName)
  if (!t || depth > 2 || seen.has(typeName)) return {}
  const next = new Set(seen).add(typeName)
  return objectFromFields(splitByLocation(t.fields).body, types, depth, next)
}

/** 手写序列化，才能输出 Postman 需要的「不带引号的 {{变量}}」。 */
function jsonOut(v, pad = 0) {
  const P = '  '.repeat(pad)
  if (v === null || v === undefined) return 'null'
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  if (typeof v === 'string') return JSON.stringify(v)
  if (Array.isArray(v)) {
    if (!v.length) return '[]'
    return '[\n' + v.map((x) => `${P}  ${jsonOut(x, pad + 1)}`).join(',\n') + `\n${P}]`
  }
  if (v.raw !== undefined) return v.raw
  const keys = Object.keys(v)
  if (!keys.length) return '{}'
  return '{\n' + keys.map((k) => `${P}  ${JSON.stringify(k)}: ${jsonOut(v[k], pad + 1)}`).join(',\n') + `\n${P}}`
}

function postmanRequest(gwName, gw, group, section, route, types, perms) {
  const full = group.prefix + route.path
  const pathSegs = full.replace(/^\//, '').split('/')
  const url = {
    raw: `${gw.baseVar}${full}`,
    host: [gw.baseVar],
    path: pathSegs.map((s) => (s.startsWith(':') ? ':' + s.slice(1) : s)),
  }
  const headers = [{ key: 'X-Trace-Id', value: '{{$guid}}', description: '链路追踪 ID（中间件按此头记入审计）' }]
  if (section.middleware === 'AdminPermission') {
    headers.push({ key: 'Authorization', value: 'Bearer {{admin_token}}', description: '运营会话令牌：POST /admin/operation/login 签发' })
  }
  const reqType = route.reqType ? types.get(route.reqType) : null
  let body
  if (reqType) {
    const parts = splitByLocation(reqType.fields)
    for (const f of parts.path) {
      const key = parseTag(f.tag).key
      const idx = url.path.findIndex((p) => p === ':' + key)
      if (idx >= 0) url.path[idx] = `{{${key}}}`
    }
    if (parts.query.length) {
      url.query = parts.query.map((f) => ({
        key: parseTag(f.tag).key || f.name,
        value: queryValue(f, types),
        description: f.note || '',
      }))
    }
    if (parts.body.length) {
      if (QUERY_METHODS.has(route.method)) {
        throw new Error(
          `${route.method} ${full}: 请求类型 ${route.reqType} 带 json 字段但 ${route.method} 没有请求体，请把入参改成 form 标签`,
        )
      }
      body = jsonOut(objectFromFields(parts.body, types))
      headers.push({ key: 'Content-Type', value: 'application/json' })
    }
  }
  // raw 必须与 path/query 里的变量替换保持一致，否则 Postman 界面会同时显示 :aid 与 {{aid}}
  url.raw =
    `${gw.baseVar}/` +
    url.path.join('/') +
    (url.query && url.query.length
      ? '?' + url.query.map((q) => `${q.key}=${q.value}`).join('&')
      : '')

  const name = `${route.method} ${full} — ${route.doc || route.handler}`
  const description = [
    `@doc：${route.doc || '（契约未写）'}`,
    `鉴权：${authOf(gwName, section.middleware, full, perms)}`,
    `handler/logic：\`${route.handler}\` → \`gateway/${gwName}/internal/logic/${logicFileOf(route.handler)}\``,
    `响应信封：\`code\`/\`message\`/\`data\`/\`ttl\` 四字段（AGENTS.md §6）。`,
  ].join('\n')

  return {
    name,
    request: {
      method: route.method,
      header: headers,
      url,
      ...(body
        ? { body: { mode: 'raw', raw: body, options: { raw: { language: 'json' } } } }
        : {}),
      description,
    },
    response: [],
    event: [
      {
        listen: 'test',
        script: {
          type: 'text/javascript',
          exec: [
            `// 只断言「统一响应信封四字段」——这是契约要求，业务成功与否由 code 决定。`,
            `pm.test('响应是统一四字段信封', function () {`,
            `  const j = pm.response.json();`,
            `  for (const k of ['code', 'message', 'data', 'ttl']) pm.expect(j).to.have.property(k);`,
            `});`,
          ],
        },
      },
    ],
  }
}

// ---------------------------------------------------------------- 输出容器

const outputs = new Map()
const emit = (rel, content) => {
  const body = content.endsWith('\n') ? content : content + '\n'
  if (outputs.has(rel)) throw new Error(`产物路径冲突：${rel}`)
  outputs.set(rel, body)
}

// ---------------------------------------------------------------- 主流程

const perms = readRoutePermissions('gateway/admin/internal/middleware/adminpermissionmiddleware.go')
const gatewayRpcKeys = new Map([
  ['app', readGatewayRpcKeys('gateway/app/etc/app.yaml')],
  ['admin', readGatewayRpcKeys('gateway/admin/etc/admin.yaml')],
])

const parsedGateways = []
for (const gw of GATEWAYS) {
  const parsed = parseApi(gw.api)
  const sections = gw_sections_with_middleware(parsed, gw)
  parsedGateways.push({ ...gw, types: parsed.types, sections })
}

/** 把 middleware 落到每条路由上，便于后续按路由分组渲染。 */
function gw_sections_with_middleware(parsed, _gw) {
  return parsed.sections.map((s) => ({ ...s, routes: s.routes.map((r) => ({ ...r, middleware: s.middleware })) }))
}

// ---- 漂移门禁：.api ↔ goctl routes.go
const driftReport = []
for (const gw of parsedGateways) {
  const fromApi = []
  for (const s of gw.sections) for (const r of s.routes) fromApi.push(keyOf(r.method, s.prefix + r.path))
  const fromGo = parseRoutesGo(gw.routes).map((r) => keyOf(r.method, r.path))
  const a = new Set(fromApi)
  const b = new Set(fromGo)
  const dupA = fromApi.length - a.size
  const dupB = fromGo.length - b.size
  const onlyApi = [...a].filter((k) => !b.has(k)).sort()
  const onlyGo = [...b].filter((k) => !a.has(k)).sort()
  if (dupA || dupB || onlyApi.length || onlyGo.length) {
    throw new Error(
      [
        `漂移门禁失败（${gw.name}）：.api 声明 ${fromApi.length} 条 / routes.go 注册 ${fromGo.length} 条`,
        dupA ? `  .api 内重复键 ${dupA} 条` : '',
        dupB ? `  routes.go 内重复键 ${dupB} 条` : '',
        onlyApi.length ? `  只在 .api 有：${onlyApi.join(' , ')}` : '',
        onlyGo.length ? `  只在 routes.go 有：${onlyGo.join(' , ')}` : '',
        `请先运行 scripts/gen.ps1 重新生成，再重跑本脚本。`,
      ]
        .filter(Boolean)
        .join('\n'),
    )
  }
  driftReport.push({ name: gw.name, count: fromApi.length, permissioned: fromApi.filter((k) => perms.has(k.split(' ')[1])).length })
}

// ---- 门禁2：.api 引用的类型必须已声明；每条路由的 handler 必须已生成 logic 文件
for (const gw of parsedGateways) {
  const badTypes = []
  const badLogic = []
  for (const s of gw.sections) {
    for (const r of s.routes) {
      for (const t of [r.reqType, r.respType]) {
        if (t && !gw.types.has(t)) badTypes.push(`${r.method} ${s.prefix}${r.path} → ${t}`)
      }
      const rel = `${gw.logicDir}/${logicFileOf(r.handler)}`
      if (!exists(rel)) badLogic.push(`${r.method} ${s.prefix}${r.path} → ${rel}`)
    }
  }
  if (badTypes.length) {
    throw new Error(`门禁失败（${gw.name}）：${badTypes.length} 条路由引用了未声明的类型\n  ${badTypes.join('\n  ')}`)
  }
  if (badLogic.length) {
    throw new Error(
      `门禁失败（${gw.name}）：${badLogic.length} 条路由没有 logic 文件（生成没跑全？）\n  ${badLogic.join('\n  ')}`,
    )
  }
}

// ---- 权限表覆盖对账（只报事实，不 fail：表内路径可能属于免中间件组的 fail-closed 空洞）
const allAdminRoutes = new Set()
for (const gw of parsedGateways.filter((g) => g.name === 'admin'))
  for (const s of gw.sections) for (const r of s.routes) allAdminRoutes.add(s.prefix + r.path)
const orphanPerms = [...perms.keys()].filter((p) => !allAdminRoutes.has(p))

// ---- HTTP 文档
const httpIndex = { app: [], admin: [] }
for (const gw of parsedGateways) {
  const groups = buildHttpGroups(gw, perms)
  const rows = []
  groups.forEach((g, idx) => {
    const num = String(idx + 1).padStart(2, '0')
    const slug = prefixSlug(g.prefix)
    const rel = `docs/api/http/${gw.name}/${num}-${slug}.md`
    emit(rel, renderHttpGroupFile(gw.name, gw, g, gw.types, perms, num))
    const auths = uniq(g.sections.map((s) => s.middleware || '免鉴权'))
    rows.push({ num, slug, prefix: g.prefix, file: rel, routes: g.routes, auths, sections: g.sections.length })
  })
  httpIndex[gw.name] = rows

  const L = []
  L.push(`# gateway/${gw.name} HTTP 接口索引`, '')
  L.push(`> ${GENERATED_NOTE}`, '')
  L.push(
    `**共 ${driftReport.find((d) => d.name === gw.name).count} 条路由，按域拆成 ${rows.length} 个文件。**`, '',
    `基址：\`http://127.0.0.1:${gw.port}\`（来自 \`${gw.etc}\` 的 \`Port\`）。`, '',
    `鉴权口径：`,
    `- 免鉴权 —— 网关无中间件。${gw.name === 'app' ? '终端身份按本仓约定由 \`mid\` 入参携带，不是可信凭据。' : '这类只读运营面刻意不进 \`routePermissions\`，取舍记录在各服务 README。'}`,
    ...(gw.name === 'admin'
      ? ['- `AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按路由权限点判定；**表外路径 fail-closed 直接拒绝**。']
      : ['- `AppkeyVerify` —— 仅 `/account/privacy`，query `appkey` 白名单。']),
    '',
    `| 文件 | 前缀 | 路由数 | 鉴权 | 小节 |`,
    `|---|---|---|---|---|`,
  )
  for (const r of rows) L.push(`| [${r.num}-${r.slug}.md](./${r.file.split('/').pop()}) | \`${r.prefix}\` | ${r.routes} | ${cell(r.auths.join(' · '))} | ${r.sections} |`)
  L.push('', '小节 = 同一前缀下按鉴权/域拆出的 `@server` 块数；文件内每个小节自带独立标题、鉴权说明与类型附录。', '')
  emit(`docs/api/http/${gw.name}/README.md`, L.join('\n'))
}

// ---- RPC 文档
const protoFiles = glob('services', (rel) => /^services\/[^/]+\/rpc\/[^/]+\.proto$/.test(rel))
const rpcRows = []
for (const rel of protoFiles) {
  const svcDir = rel.split('/')[1]
  const proto = parseProto(rel)
  const conf = readServiceConf(svcDir)
  const consumers = []
  for (const [gwName, map] of gatewayRpcKeys) {
    const field = map.get(conf.regKey)
    if (field) consumers.push(`${gwName}:${field}`)
  }
  const methods = proto.services.flatMap((s) => s.methods)
  const byName = new Map(proto.types.map((t) => [t.name, t]))
  const simpleNames = new Map()
  for (const t of proto.types) if (!simpleNames.has(t.simpleName)) simpleNames.set(t.simpleName, t)

  const L = []
  L.push(`# RPC · \`${svcDir}\``, '')
  L.push(`> ${GENERATED_NOTE}`, '')
  L.push(
    `| 项 | 值 |`, `|---|---|`,
    `| 契约文件 | \`${rel}\` |`,
    `| protobuf 包 | \`${proto.package}\` |`,
    `| go_package | \`${proto.goPackage}\` |`,
    `| 发现用的 etcd key | \`${conf.regKey}\`（\`${conf.file}\` 顶层 \`Etcd.Key\`，网关要命中这个值） |`,
    conf.name === conf.regKey
      ? `| 配置里的 \`Name\` | 与上面的 key 相同（\`${conf.name}\`） |`
      : `| 配置里的 \`Name\` | \`${conf.name}\`——**与注册 key 不同**，按 \`Name\` 连不上本服务 |`,
    `| 监听 | \`${conf.port || '—'}\`（\`${conf.file}\` 的 \`ListenOn\`） |`,
    conf.dsn ? `| 数据库 | \`${conf.dsn}\` |` : `| 数据库 | 无独立库（不落 MySQL） |`,
    `| 方法数 | ${methods.length}（service ${proto.services.map((s) => `\`${s.name}\``).join('、') || '—'}） |`,
    `| 网关消费方 | ${consumers.length ? consumers.map((c) => `\`${c}\``).join('、') : '未被两个网关的 etcd key 引用（服务间调用或本轮未接线）'} |`,
    '',
  )
  if (proto.header.length) {
    L.push(`## 契约说明`, '')
    L.push(...proto.header.map((h) => (h ? `> ${h}` : '>')), '')
  }
  /** 把 protobuf 类型名渲染成指向本文内定义的链接（标量原样输出）。 */
  const anchorOf = (t) => `#${(t.kind + '-' + t.name).toLowerCase().replace(/[`./]/g, '').replace(/\s+/g, '-')}`
  const typeCell = (raw) => {
    const bare = String(raw).replace(/^(?:repeated|optional)\s+/, '')
    const mp = bare.match(/^map<[^,]+,\s*([\w.]+)>$/)
    const token = mp ? mp[1] : bare
    const t = byName.get(token) || simpleNames.get(token)
    if (!t) return `\`${cell(bare)}\``
    const label = mp ? bare.replace(token, t.name) : t.name
    return `[\`${cell(label)}\`](${anchorOf(t)})`
  }
  const fieldRow = (f) =>
    `| \`${f.name}\` | ${typeCell(f.type)} | ${f.num} | ${cell([f.label, f.opts && f.opts.replace(/[\[\]]/g, '')].filter(Boolean).join(' '))} | ${cell(f.comment)} |`

  for (const s of proto.services) {
    L.push(`## service \`${s.name}\``, '')
    if (s.comment) L.push(`> ${s.comment}`, '')
    L.push(`gRPC 方法前缀：\`${proto.package}.${s.name}/\``, '')
    L.push(`| # | 方法 | 请求 | 响应 | 说明 |`, `|---|---|---|---|---|`)
    s.methods.forEach((m, i) => {
      const streams = [m.clientStream && 'client-stream', m.serverStream && 'server-stream'].filter(Boolean).join('+')
      L.push(
        `| ${i + 1} | \`${m.name}\` | ${typeCell(m.in)} | ${typeCell(m.out)} | ${cell(m.comment)}${streams ? ` *(流式：${streams})*` : ''} |`,
      )
    })
    L.push('')
  }
  if (proto.types.length) {
    L.push(`## 消息与枚举`, '')
    for (const t of proto.types) {
      if (t.kind === 'message') {
        L.push(`### message \`${t.name}\``, '')
        if (t.comment) L.push(`> ${t.comment}`, '')
        if (!t.fields.length) { L.push('（空消息）', ''); continue }
        L.push(`| 字段 | 类型 | 编号 | 修饰 | 说明 |`, `|---|---|---|---|---|`)
        for (const f of t.fields) L.push(fieldRow(f))
        L.push('')
      } else {
        L.push(`### enum \`${t.name}\``, '')
        if (t.comment) L.push(`> ${t.comment}`, '')
        L.push(`| 值 | 编号 | 说明 |`, `|---|---|---|`)
        for (const v of t.values) L.push(`| \`${v.name}\` | ${v.num} | ${cell(v.comment)} |`)
        L.push('')
      }
    }
  }
  const docRel = `docs/api/rpc/${svcDir}.md`
  emit(docRel, L.join('\n'))
  rpcRows.push({ svcDir, docRel, pkg: proto.package, name: conf.name, regKey: conf.regKey, port: conf.port, methods: methods.length, file: rel, consumers })
}

// ---- 门禁：网关引用的每个 etcd key 都必须命中某个服务自己的注册 key（写错时编译期与单测都不暴露，只有真连 etcd 才炸）
{
  const regKeys = new Set(rpcRows.map((r) => r.regKey).filter(Boolean))
  const dangling = []
  for (const [gwName, map] of gatewayRpcKeys) {
    for (const [key, field] of map) if (!regKeys.has(key)) dangling.push(`${gwName}:${field} → ${key}`)
  }
  if (dangling.length) {
    throw new Error(
      `门禁失败（服务发现）：网关配置里 ${dangling.length} 个 etcd key 对不上任何服务的注册 key\n  ${dangling.join('\n  ')}`,
    )
  }
}

// ---- RPC 面归属：由「哪个网关的哪个配置字段引用了这个 etcd key」派生，不手写领域表
const RPC_PLANES = [
  { key: 'both', title: '终端面与运营面都消费', why: '同一份契约既服务客户端用例，也被后台排障/运营页调用；改它要同时守两条兼容线。' },
  { key: 'app', title: '只有终端面消费', why: '面向 Android/iOS/HarmonyOS/桌面的读用例，版本兼容窗口最长，不能随意改字段语义。' },
  { key: 'admin', title: '只有运营面消费', why: '只被 `gateway/admin` 引用；鉴权与权限点判定在网关侧，契约里通常带 operator 位。' },
  { key: 'internal', title: '两个网关都不引用（服务间 / worker / 尚未接线）', why: '没有 HTTP 入口，只由其它服务或定时任务调用；冒烟时需要单独起进程。' },
]
const planeOf = (consumers) => {
  const app = consumers.some((c) => c.startsWith('app:'))
  const admin = consumers.some((c) => c.startsWith('admin:'))
  return app && admin ? 'both' : app ? 'app' : admin ? 'admin' : 'internal'
}
const planeBySvc = new Map()
for (const r of rpcRows) planeBySvc.set(r.svcDir, planeOf(r.consumers))

// ---- RPC 索引
{
  const L = []
  L.push(`# RPC 契约索引（${rpcRows.length} 个服务 / ${rpcRows.reduce((a, b) => a + b.methods, 0)} 个方法）`, '')
  L.push(`> ${GENERATED_NOTE}`, '')
  L.push(`每个服务一个文件，含方法表、消息与枚举全量定义、etcd key、端口与网关消费方。`, '')
  L.push(
    `下面按**消费方**分节（不是按字母顺序摊平）：某服务被哪个网关的哪个配置字段引用，`,
    '是从两个网关的 `etc/*.yaml` 与 `internal/config/*.go` 读出来的，不是手工归类。', '',
  )
  const missing = rpcRows.filter((r) => !RPC_PLANES.some((p) => p.key === planeBySvc.get(r.svcDir)))
  if (missing.length) throw new Error(`RPC 索引门禁失败：${missing.length} 个服务没有归属面分组\n  ${missing.map((m) => m.svcDir).join('\n  ')}`)
  const seen = new Set()
  for (const plane of RPC_PLANES) {
    const rows = rpcRows.filter((r) => planeBySvc.get(r.svcDir) === plane.key).sort((a, b) => a.svcDir.localeCompare(b.svcDir))
    if (!rows.length) continue
    for (const r of rows) seen.add(r.svcDir)
    L.push(
      `## ${plane.title}（${rows.length} 个服务 / ${rows.reduce((a, b) => a + b.methods, 0)} 个方法）`, '',
      plane.why, '',
      `| 服务 | 文档 | proto 包 | 发现用的 etcd key | 端口 | 方法 | 网关消费方 |`, `|---|---|---|---|---|---|---|`,
    )
    for (const r of rows) {
      const key = r.regKey === r.name ? `\`${r.regKey}\`` : `\`${r.regKey}\`（配置里 \`Name\` 写的是 \`${r.name}\`）`
      L.push(
        `| \`${r.svcDir}\` | [${r.svcDir}.md](./${r.svcDir}.md) | \`${r.pkg}\` | ${key} | ${r.port} | ${r.methods} | ${r.consumers.length ? cell(r.consumers.join('、')) : '—'} |`,
      )
    }
    L.push('')
  }
  const ungrouped = rpcRows.filter((r) => !seen.has(r.svcDir))
  if (ungrouped.length) throw new Error(`RPC 索引门禁失败：${ungrouped.length} 个服务漏在分组之外\n  ${ungrouped.map((u) => u.svcDir).join('\n  ')}`)
  L.push(`调用方式见 [scripts/rpc/README.md](../../../scripts/rpc/README.md)。`, '')
  emit('docs/api/rpc/README.md', L.join('\n'))
}

// ---- Postman
const appGw = parsedGateways.find((g) => g.name === 'app')
const adminGw = parsedGateways.find((g) => g.name === 'admin')

// 集合内引用的 {{var}} 全部由此表给本地默认值；表外的 *_id 退化为 '1'。
const ENV_KNOWN = new Map([
  ['appUrl', 'http://127.0.0.1:8080'],
  ['adminUrl', 'http://127.0.0.1:8081'],
  ['admin_token', ''],
  ['clientIp', '127.0.0.1'],
  ['appkey', 'android'],
  ['bvid', 'BV1xxxxxxxxx'],
  ['mid', '1'],
  ['aid', '1'],
  ['device_id', 'device-demo-001'],
])
const envValue = (k) => ENV_KNOWN.get(k) ?? (/(id|_id)$/.test(k) ? '1' : '')
// Postman 内置变量（{{$guid}} / {{$uuid}}）由 Postman 自己生成，不进环境变量表
const referencedVars = (json) => [
  ...new Set([...json.matchAll(/\{\{([a-zA-Z_][\w]*)\}\}/g)].map((m) => m[1])),
].sort()

for (const gw of [appGw, adminGw]) {
  const groups = buildHttpGroups(gw, perms)
  // 一个域分组 = 一个 folder；同一前缀下有多个 @server 段时再拆子 folder，不把不同鉴权域的用例揉平
  const folders = groups.map((g) => ({
    name: `${g.prefix}（${g.routes} 条）`,
    description: g.sections.map((s) => `${s.title || '路由组'} · ${s.middleware || '免鉴权'}`).join('\n'),
    item:
      g.sections.length === 1
        ? g.sections[0].routes.map((r) => postmanRequest(gw.name, gw, g, g.sections[0], r, gw.types, perms))
        : g.sections.map((s) => ({
            name: `${s.title || '路由组'}（${s.routes.length} 条 · ${s.middleware || '免鉴权'}）`,
            description: `鉴权：${s.middleware ? `\`${s.middleware}\`` : '免鉴权（网关不校验会话/权限点）'}`,
            item: s.routes.map((r) => postmanRequest(gw.name, gw, g, s, r, gw.types, perms)),
          })),
  }))
  const collection = {
    info: {
      name: `go-video · gateway/${gw.name}`,
      _postman_id: `go-video-${gw.name}`,
      description: [
        `由 \`node scripts/gen-api-docs.mjs\` 从 \`gateway/${gw.api.replace('gateway/', '')}\` 生成，请勿手工编辑。`,
        `folder = HTTP 域分组（按 .api 前缀），共 ${folders.length} 组 / ${groups.reduce((a, g) => a + g.routes, 0)} 条请求。`,
        `集合级断言只校验统一响应信封四字段，不断言业务 code。`,
      ].join('\n'),
      schema: 'https://schema.getpostman.com/json/collection/v2.1.0/collection.json',
    },
    item: folders,
    variable: referencedVars(JSON.stringify(folders))
      .filter((v) => v !== (gw.name === 'app' ? 'adminUrl' : 'appUrl'))
      .map((v) => ({ key: v, value: envValue(v) })),
  }
  emit(`postman/go-video-${gw.name}.postman_collection.json`, JSON.stringify(collection, null, 2))
}

// 环境 = 两个集合真正引用到的变量的并集，避免「集合用了变量但环境没给值」
{
  const all = new Set()
  for (const [rel, content] of outputs) {
    if (!rel.startsWith('postman/go-video-')) continue
    for (const v of referencedVars(content)) all.add(v)
  }
  emit(
    'postman/environments/go-video-local.postman_environment.json',
    JSON.stringify(
      {
        name: 'go-video local',
        values: [...all]
          .sort()
          .map((k) => ({
            key: k,
            value: envValue(k),
            type: k === 'admin_token' ? 'secret' : 'string',
            enabled: true,
          })),
        _postman_variable_scope: 'environment',
      },
      null,
      2,
    ),
  )
}

// ---- RPC 冒烟脚本
function protoSkeleton(msgName, typesByName, depth = 0, seen = new Set()) {
  const t = typesByName.get(msgName)
  if (!t || !t.fields.length) return {}
  if (depth > 1 || seen.has(msgName)) return {}
  const next = new Set(seen).add(msgName)
  const obj = {}
  for (const f of t.fields) {
    const label = f.label || ''
    let v
    if (label === 'repeated') {
      if (f.type.startsWith('map<')) v = [{}]
      else if (typesByName.has(f.type)) v = [protoSkeleton(f.type, typesByName, depth + 1, next)]
      else v = [scalarSample(f.type)]
    } else if (f.type.startsWith('map<')) {
      const vt = f.type.match(/^map<[^,]+,\s*([\w.]+)>$/)?.[1] || ''
      v = typesByName.has(vt) ? { k: protoSkeleton(vt, typesByName, depth + 1, next) } : { k: scalarSample(vt) }
    } else if (typesByName.has(f.type)) {
      v = protoSkeleton(f.type, typesByName, depth + 1, next)
    } else {
      v = scalarSample(f.type)
    }
    obj[f.name] = v
  }
  return obj
}

function scalarSample(t) {
  if (t === 'bool') return false
  if (t === 'string') return ''
  if (t === 'bytes') return ''
  if (/int|double|float/.test(t)) return 0
  return null
}

const smokeServices = []
for (const rel of protoFiles) {
  const svcDir = rel.split('/')[1]
  const proto = parseProto(rel)
  const conf = readServiceConf(svcDir)
  if (!conf.port || !proto.services.length) continue
  const typesByName = new Map(proto.types.map((t) => [t.name, t]))
  const bySimple = new Map()
  for (const t of proto.types) if (!bySimple.has(t.simpleName)) bySimple.set(t.simpleName, t)
  const resolve = (n) => (typesByName.has(n) ? n : bySimple.has(n) ? bySimple.get(n).name : null)
  const calls = []
  for (const s of proto.services) {
    for (const m of s.methods) {
      if (m.clientStream || m.serverStream) { calls.push({ skip: `流式方法不做冒烟：${s.name}/${m.name}` }); continue }
      const req = resolve(m.in)
      const body = req ? protoSkeleton(req, typesByName) : {}
      calls.push({
        full: `${proto.package}.${s.name}/${m.name}`,
        json: JSON.stringify(body),
        comment: m.comment,
      })
    }
  }
  smokeServices.push({ svcDir, port: conf.port, calls })
}

{
  const total = smokeServices.reduce((a, s) => a + s.calls.filter((c) => !c.skip).length, 0)
  const hdr = [
    `契约真源：services/*/rpc/*.proto（${protoFiles.length} 份）。`,
    `覆盖 ${smokeServices.length} 个服务、${total} 个 RPC 方法；先按消费面分大节，节内再按服务分节。`,
    '',
    '状态：本脚本**从未在本仓执行过**——这里没有运行中的服务进程、没有 etcd，',
    '维护者机器上的 MySQL 也不允许被自动化触碰。它只是把「每个方法怎么调」固化成',
    '可复用入口；请求体是 proto 字段骨架，**不含业务前置数据**，绝大多数调用会因',
    '记录不存在/参数校验失败而返回错误，这属于预期。不要把它当成通过/失败断言。',
    '',
    '前置：`grpcurl` 在 PATH（或 GRPCURL 环境变量指到可执行文件），目标服务已启动，',
    '并且用 `-d` 里的占位值换成真实存在的主键才能观察到成功分支。',
    '脚本靠 reflection 解析方法名：服务的 services/*/*.v1.go 只在 Mode 为 dev/test 时',
    'reflection.Register（示例 etc/*.yaml 都是 Mode: dev，本地默认可用）。',
    '若目标进程是 Mode: pro，反射不会注册，需要改成',
    'grpcurl -import-path services/<svc>/rpc -proto <svc>.proto 的显式模式。',
  ]

  const sh = [
    `#!/usr/bin/env bash`,
    ...hdr.map((h) => `# ${h}`.replace(/ `([^`]+)` /g, ' $1 ')),
    `set -uo pipefail`,
    `GRPCURL="\${GRPCURL:-grpcurl}"`,
    `HOST="\${RPC_HOST:-127.0.0.1}"`,
    `PASS=0; FAIL=0`,
    `rpc() { # rpc <port> <package.Service/Method> <json>`,
    `  printf '==> %s :%s\\n' "$2" "$1"`,
    `  if "$GRPCURL" -plaintext -d "$3" "$HOST:$1" "$2" 2>&1; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); fi`,
    `}`,
    ``,
  ]
  const ps = [
    `# ${hdr.join('\n# ')}`,
    `$ErrorActionPreference = 'Continue'`,
    `$GRPCURL = if ($env:GRPCURL) { $env:GRPCURL } else { 'grpcurl' }`,
    `$Host_ = if ($env:RPC_HOST) { $env:RPC_HOST } else { '127.0.0.1' }`,
    `$PASS = 0; $FAIL = 0`,
    `function Rpc {`,
    `  param([int]$Port, [string]$Method, [string]$Body)`,
    `  Write-Host "==> $Method (:$Port)"`,
    `  & $GRPCURL -plaintext -d $Body "$($Host_):$Port" $Method 2>&1`,
    `  if ($LASTEXITCODE -eq 0) { $PASS++ } else { $FAIL++ }`,
    `}`,
    ``,
  ]
  // 按「谁消费这份契约」分大节，再在节内按服务分小节日；归属由两个网关的配置派生
  const orphan = smokeServices.filter((s) => !planeBySvc.has(s.svcDir))
  if (orphan.length) throw new Error(`冒烟脚本门禁失败：${orphan.length} 个服务查不到消费方归属\n  ${orphan.map((o) => o.svcDir).join('\n  ')}`)
  const planeGroups = []
  for (const plane of RPC_PLANES) {
    const group = smokeServices
      .filter((s) => planeBySvc.get(s.svcDir) === plane.key)
      .sort((a, b) => a.svcDir.localeCompare(b.svcDir))
    if (group.length) planeGroups.push({ plane, group })
  }
  if (planeGroups.reduce((a, g) => a + g.group.length, 0) !== smokeServices.length) {
    throw new Error('冒烟脚本门禁失败：分组后的服务数与总服务数不一致（有服务被重复归类或漏归类）')
  }
  for (const { plane, group } of planeGroups) {
    const banner = `# @@ 消费面：${plane.title} —— ${group.length} 个服务 / ${group.reduce((a, s) => a + s.calls.filter((c) => !c.skip).length, 0)} 个方法 @@`
    sh.push(banner, ``)
    ps.push(banner, ``)
    for (const s of group) {
      sh.push(`# ---------- ${s.svcDir} (端口 ${s.port}) ----------`)
      ps.push(`# ---------- ${s.svcDir} (端口 ${s.port}) ----------`)
      for (const c of s.calls) {
        if (c.skip) { sh.push(`# skip: ${c.skip}`); ps.push(`# skip: ${c.skip}`); continue }
        const body = c.json.replace(/'/g, `'\\''`)
        sh.push(`# ${c.comment || c.full}`)
        sh.push(`rpc ${s.port} '${c.full}' '${body}'`)
        ps.push(`# ${c.comment || c.full}`)
        ps.push(`Rpc -Port ${s.port} -Method '${c.full}' -Body '${body.replace(/'/g, "''")}'`)
      }
      sh.push('')
      ps.push('')
    }
  }
  sh.push(`echo "调用数：成功返回 $PASS / 非零退出 $FAIL（非零退出≠缺陷，见文件头说明）"`)
  ps.push(`Write-Host "调用数：成功返回 $PASS / 非零退出 $FAIL（非零退出≠缺陷，见文件头说明）"`)
  emit('scripts/rpc/smoke.sh', sh.join('\n'))
  emit('scripts/rpc/smoke.ps1', ps.join('\n'))

  const L = []
  L.push(`# RPC 接口冒烟脚本`, '')
  L.push(`> ${GENERATED_NOTE}`, '')
  L.push(...hdr.map((h) => h ? h : ''), '')
  L.push(`| 脚本 | 说明 |`, `|---|---|`,
    `| \`smoke.sh\` | Bash + grpcurl（plaintext，走 reflection；\`Mode: pro\` 的进程要改加 \`-import-path -proto\`） |`,
    `| \`smoke.ps1\` | PowerShell 等价实现，Windows 本地开发用 |`, '')
  const streamed = smokeServices.reduce((a, s) => a + s.calls.filter((c) => c.skip).length, 0)
  L.push(
    `覆盖：${smokeServices.length} 个服务 / ${total} 个方法。`,
    streamed
      ? `另有 ${streamed} 个流式方法只以 \`# skip:\` 注释标出，不发请求。`
      : `契约里没有任何流式方法（proto 无 \`stream\`），所以每条都是单次调用。`, '',
  )
  L.push(
    `## 分节口径（先按消费面，再按服务）`, '',
    `一个大节以 \`# @@ 消费面：… @@\` 横幅开始，节内每个服务一个 ` + '`----------`' + ` 小节日。`,
    '归属不是手工归类，而是读两个网关的 `etc/*.yaml` 与 `internal/config/*.go`：',
    `看哪个网关的哪个 RPC 配置字段引用了这个服务的注册 etcd key。`, '',
    `| 大节 | 服务 | 方法 | 服务清单 |`, `|---|---|---|---|`,
  )
  for (const { plane, group } of planeGroups) {
    L.push(
      `| ${plane.title} | ${group.length} | ${group.reduce((a, s) => a + s.calls.filter((c) => !c.skip).length, 0)} | ${group.map((s) => `\`${s.svcDir}\``).join('、')} |`,
    )
  }
  L.push(
    '',
    `只跑一个大节：按横幅切出来再执行（横幅用 ` + '`@@`' + ` 包起来，不会被 proto 注释里的 \`====\` 误匹配）`, '',
    '```bash',
    `awk '/^# @@ 消费面：/{p=0} /^# @@ 消费面：.*只有运营面消费/{p=1} p' scripts/rpc/smoke.sh | bash`,
    '```', '',
    `逐方法的字段定义见 [docs/api/rpc/](../../docs/api/rpc/README.md)（同一套分节口径）。`, '',
  )
  L.push(`## 与单测的分工`, '')
  L.push(
    `- Go 侧单测（\`services/*/internal/logic/*_test.go\`）用替身驱动，是 CI 门禁的一部分，能真的失败；`,
    `- 本脚本是**联调工具**，需要真实进程与 etcd，输出只作观察记录；`,
    `- 两者都跑过之前，任何 README 都不应写「RPC 已实机联调」。`, '',
  )
  emit('scripts/rpc/README.md', L.join('\n'))
}

// ---- 总索引
{
  const appCount = driftReport.find((d) => d.name === 'app').count
  const adminCount = driftReport.find((d) => d.name === 'admin').count
  const rpcMethods = rpcRows.reduce((a, b) => a + b.methods, 0)
  const L = []
  L.push(`# 接口文档（HTTP / RPC / Postman / 冒烟脚本）`, '')
  L.push(`> ${GENERATED_NOTE}`, '')
  L.push(`## 目录结构`, '')
  L.push(
    '```text',
    'docs/api/',
    '├── http/           对外 HTTP 面，按域分文件',
    `│   ├── app/        gateway/app —— ${appCount} 条路由 / ${httpIndex.app.length} 个分组文件`,
    `│   └── admin/      gateway/admin —— ${adminCount} 条路由 / ${httpIndex.admin.length} 个分组文件`,
    `└── rpc/            内部 RPC 面，一服务一文件 —— ${rpcRows.length} 服务 / ${rpcMethods} 方法`,
    '```', '',
    `配套产物：`,
    `- Postman：[\`postman/\`](../../postman/README.md)（两个 collection，folder 与本文档分组一一对应）`,
    `- RPC 冒烟：[\`scripts/rpc/\`](../../scripts/rpc/README.md)（**未执行过**，需要真实进程）`, '',
    `## 重新生成`, '')
  L.push('```powershell', 'node scripts/gen-api-docs.mjs          # 生成并写盘', 'node scripts/gen-api-docs.mjs --check   # 只校验：漂移门禁 + 产物是否最新', '```', '')
  L.push(
    `## 阅读前要知道的四件事`, '',
    `1. **HTTP 响应统一信封**：所有 \`/api\`、\`/admin\`、\`/x\` 面的成功与业务错误都返回 \`code\`/\`message\`/\`data\`/\`ttl\`；`,
    `   文档里的响应类型表自带这四个字段，\`data\` 内层结构在各文件的「类型附录」中展开（AGENTS.md §6）。`,
    `2. **鉴权不是全局的**：\`gateway/app\` 没有 JWT 中间件，\`mid\` 只是入参约定；\`gateway/admin\` 只有挂了`,
    `   \`AdminPermission\` 的分组才校验 Bearer 会话与权限点，其余分组免鉴权（刻意如此，理由写在分组文件里）。`,
    `3. **权限点覆盖情况**（本轮实测）：admin 受保护路由 ${driftReport.find((d) => d.name === 'admin').permissioned} 条已在`,
    `   \`routePermissions\` 登记；权限表内路径全部能在 .api 中找到对应路由${orphanPerms.length ? `（例外：${orphanPerms.join(', ')}）` : ''}。`,
    `   角色↔权限点绑定不在迁移里，新库仍需运营手工授权。`,
    `4. **入参位置决定编码**：\`path\`→路径段，\`form\`→URL 查询串（POST 也一样），\`json\`→JSON 请求体。`,
    `   依据是 go-zero \`httpx.Parse\` 会同时跑 \`ParseForm\` 与 \`ParseJsonBody\`；结构体数组不能挂在 \`form\` 上，`,
    `   细节和实测结论见 [postman/README.md](../../postman/README.md) 的「入参编码」一节。`, '',
    `## 快速跳转`, '',
    `### 终端面（${appCount} 条）`, '',
    `| 分组 | 前缀 | 路由 | 鉴权 |`, `|---|---|---|---|`,
  )
  for (const r of httpIndex.app) L.push(`| [${r.num}-${r.slug}](${r.file.replace('docs/api/', '')}) | \`${r.prefix}\` | ${r.routes} | ${cell(r.auths.join(' · '))} |`)
  L.push('', `### 运营面（${adminCount} 条）`, '', `| 分组 | 前缀 | 路由 | 鉴权 |`, `|---|---|---|---|`)
  for (const r of httpIndex.admin) L.push(`| [${r.num}-${r.slug}](${r.file.replace('docs/api/', '')}) | \`${r.prefix}\` | ${r.routes} | ${cell(r.auths.join(' · '))} |`)
  L.push('', `### RPC（${rpcRows.length} 个服务）`, '', `[按消费面分节的完整索引](./rpc/README.md)（终端面/运营面/两面/无网关消费方）`, '')
  emit('docs/api/README.md', L.join('\n'))
}

// ---- postman README
{
  const cols = ['app', 'admin'].map((n) => JSON.parse(outputs.get(`postman/go-video-${n}.postman_collection.json`)))
  const shape = (c) => {
    let top = 0
    let sub = 0
    let reqs = 0
    let jsonBody = 0
    let queryOnly = 0
    let bare = 0
    const walk = (items, depth) => {
      for (const it of items) {
        if (it.request) {
          reqs++
          const u = it.request.url
          if (it.request.body) jsonBody++
          else if (typeof u !== 'string' && u.query && u.query.length) queryOnly++
          else bare++
        } else if (it.item) {
          if (depth === 0) top++
          else sub++
          walk(it.item, depth + 1)
        }
      }
    }
    walk(c.item, 0)
    return { top, sub, reqs, jsonBody, queryOnly, bare }
  }
  const envVars = JSON.parse(outputs.get('postman/environments/go-video-local.postman_environment.json')).values
  const exampleSub = (cols[1].item.find((f) => (f.item || []).some((s) => s.item))?.item || []).find((s) => s.item)?.name || ''
  const L = []
  const sa = shape(cols[0])
  const sb = shape(cols[1])
  L.push(`# Postman 接口测试集合`, '')
  L.push(`> ${GENERATED_NOTE}`, '')
  L.push(
    `| 文件 | 内容 |`, `|---|---|`,
    `| \`go-video-app.postman_collection.json\` | 终端面 ${sa.reqs} 条请求，${sa.top} 个 folder + ${sa.sub} 个子 folder |`,
    `| \`go-video-admin.postman_collection.json\` | 运营面 ${sb.reqs} 条请求，${sb.top} 个 folder + ${sb.sub} 个子 folder |`,
    `| \`environments/go-video-local.postman_environment.json\` | ${envVars.length} 个本地变量，由两个集合实际引用到的 \`{{var}}\` 反查生成 |`, '',
    `请求条数与 \`routes.go\` 注册数由同一份 \`.api\` 派生，脚本用漂移门禁把守（app ${driftReport.find((d) => d.name === 'app').count} / admin ${driftReport.find((d) => d.name === 'admin').count}）。`, '',
    `## 分组口径`, '',
    `folder 与 [接口文档索引](../docs/api/README.md) 的分组文件一一对应：顶层 folder = \`.api\` 里的 \`@server prefix\`；`,
    `同一前缀下有多个 \`@server\` 段（鉴权域不同）时再拆子 folder，子 folder 名带路由条数与中间件名，例如 \`${exampleSub}\`。`,
    `**没有把所有接口平铺成一个 folder。**`, '',
    `## 导入`, '',
    `1. Postman → Import → 选两个 collection 与 \`environments/go-video-local.postman_environment.json\`。`,
    `2. 右上角环境切到 \`go-video local\`。`,
    `3. 终端面服务跑在 \`:8080\`、运营面 \`:8081\`（端口来自 \`gateway/*/etc/*.yaml\`）。`, '',
    `## 变量`, '',
    `下表由环境文件本身反查生成，不是手写清单：`, '',
    `| 变量 | 本地默认值 | 类型 |`, `|---|---|---|`,
  )
  for (const v of envVars) {
    L.push(`| \`${v.key}\` | ${v.value ? `` + '`' + v.value + '`' : '（空，需手工回填）'} | ${v.type} |`)
  }
  L.push(
    '',
    `语义提醒：`, '',
    `- \`mid\` 是 app 面的身份**入参约定**，不是可信凭据（app 网关没有 JWT 中间件）；`,
    `- \`appkey\` 只用于 \`/account/privacy\` 的 \`AppkeyVerify\` 白名单，白名单未配置时中间件放行；`,
    `- \`admin_token\` 需先跑 \`POST {{adminUrl}}/admin/operation/login\` 再手工回填，否则 \`AdminPermission\` 分组按 fail-closed 拒绝；`,
    `- 其余 \`*_id\` / \`aid\` / \`bvid\` 之类是路径与查询主键占位，指向库里真实数据前只会命中「记录不存在」分支。`, '',
    `## 入参编码（决定请求怎么发）`, '',
    `go-zero 的 \`httpx.Parse\` 对每个请求依次做 \`ParseForm\`（读 \`r.Form\`，含 URL 查询串）和 \`ParseJsonBody\`（只在 Content-Type 含 \`json\` 时读体），`,
    `所以本集合按 \`.api\` 的标签位置分发参数：\`path\`→路径段、\`form\`→查询串、\`json\`→JSON 体。实测口径：`, '',
    `- urlencoded 请求体**不会**喂给 \`json\` 字段（\`mid=7&name=x\` 会报 \`field "name" is not set\`）；`,
    `- \`form\` 字段可以从查询串取到，POST 也一样（\`/m?mid=7\` + \`{"name":"x"}\` 两个字段都能绑定）；`,
    `- \`form\` 标签的结构体数组绑不上（\`type mismatch for field "parts"\`），这类字段必须用 \`json\`，`,
    `  终端面 \`POST /upload/complete\` 的分片清单就是按这条修正过的。`, '',
    `集合里的实际分布：`, '',
    `| 集合 | JSON 体 | 只有查询串 | 无入参 |`, `|---|---|---|---|`,
    `| 终端面 | ${sa.jsonBody} | ${sa.queryOnly} | ${sa.bare} |`,
    `| 运营面 | ${sb.jsonBody} | ${sb.queryOnly} | ${sb.bare} |`, '',
    `## 请求体与 URL 的其它约定`, '',
    `- 数字型变量在 JSON 模板里**故意不带引号**（如 \`"mid": {{mid}}\`）：go-zero 的 int64 绑定不接受字符串，`,
    `  因此直接 \`JSON.parse\` 未替换的模板会报错，Postman 先替换变量再发送，实际发出的报文是合法 JSON。`,
    `- 幂等键（\`biz_no\`/\`idempotency_key\`/\`out_trade_no\` 等）用 Postman 内置 \`{{$uuid}}\`，\`X-Trace-Id\` 用 \`{{$guid}}\`，每次运行都不同。`,
    `- \`url.raw\` 与 \`path[]\` 由同一份替换结果生成，不会同时出现 \`:aid\` 和 \`{{aid}}\`。`, '',
    `## 断言口径`, '',
    `每个请求只带一条断言：**响应必须是统一四字段信封**（\`code\`/\`message\`/\`data\`/\`ttl\`）。`,
    `刻意不断言 \`code == 0\`：集合要能在未造数据的库上跑通，业务码非 0 是正常结果。`,
    `需要业务断言时按用例另建 folder，不要改本文件（会被下次生成覆盖）。`, '',
    `## 已知边界`, '',
    `- 集合与真实库数据无关，跑通 ≠ 接口正确；本仓库未连接任何数据库或网关执行过这套集合；`,
    `- \`AdminPermission\` 分组未带令牌时会被 fail-closed 拒绝，这是预期行为；`,
    `- 角色↔权限点绑定不在迁移种子内，新库需运营先建角色授权，否则受保护路由一律 403；`,
    `- 契约里没有 WebSocket/SSE 路由（\`.api\` 不支持声明），因此集合只有「一次请求一次响应」的用例；上传链路只覆盖申请预签名这类 HTTP 调用，对象存储侧的真实 PUT 不在集合内。`, '',
  )
  emit('postman/README.md', L.join('\n'))
}

// ---------------------------------------------------------------- 写盘 / 校验

// 门禁3：产物内部的相对链接必须指向真实文件（同一批产物或磁盘上的既有文件）
{
  const broken = []
  for (const [rel, content] of outputs) {
    if (!rel.endsWith('.md')) continue
    for (const m of content.matchAll(/\]\((?!https?:|mailto:|#)([^)#]+)(#[^)]*)?\)/g)) {
      const target = path.posix.normalize(path.posix.join(path.posix.dirname(rel), m[1]))
      if (!outputs.has(target) && !exists(target)) broken.push(`${rel} → ${m[1]}`)
    }
  }
  if (broken.length) {
    throw new Error(`内部链接失效 ${broken.length} 处：\n  ` + broken.slice(0, 20).join('\n  '))
  }
}

if (CHECK) {
  const stale = []
  for (const [rel, body] of outputs) {
    if (!exists(rel) || read(rel) !== body) stale.push(rel)
  }
  if (stale.length) {
    console.error(`--check 失败：${stale.length} 个产物与契约不一致或缺失`)
    for (const s of stale.slice(0, 40)) console.error(`  ${s}`)
    process.exit(1)
  }
  console.log(`--check 通过：漂移门禁 OK（app ${driftReport[0].count} / admin ${driftReport[1].count}），${outputs.size} 个产物均为最新`)
  process.exit(0)
}

let written = 0
for (const [rel, body] of outputs) {
  const abs = path.join(ROOT, rel)
  fs.mkdirSync(path.dirname(abs), { recursive: true })
  fs.writeFileSync(abs, body, 'utf8')
  written++
}
console.log(
  `生成 ${written} 个产物：HTTP app ${driftReport[0].count} 条 / admin ${driftReport[1].count} 条（漂移门禁通过），` +
    `RPC ${rpcRows.length} 服务 / ${rpcRows.reduce((a, b) => a + b.methods, 0)} 方法。`,
)
