import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(new URL('../../internal/standalone/web/operations.js', import.meta.url), 'utf8');
const settle = async () => { for (let i = 0; i < 5; i++) await new Promise(r => setImmediate(r)); };
const deferred = () => { let resolve; const promise = new Promise(r => resolve = r); return {promise, resolve}; };
class Element {
  constructor(tag) { this.tagName = tag; this.children = []; this.handlers = new Map(); this.attributes = {}; this.hidden = false; this.value = ''; this.content = ''; }
  append(...children) { this.children.push(...children); }
  prepend(...children) { this.children.unshift(...children); }
  replaceChildren(...children) { this.children = children; this.content = ''; }
  set textContent(value) { this.content = value; this.children = []; }
  get textContent() { return this.content + this.children.map(c => c.textContent).join(''); }
  setAttribute(k,v) { this.attributes[k] = v; }
  addEventListener(type, handler) { this.handlers.set(type, [...(this.handlers.get(type) || []), handler]); }
  async emit(type) { for (const handler of this.handlers.get(type) || []) await handler({preventDefault(){}}); await settle(); }
  showModal() { this.open = true; }
  close() { this.open = false; for (const handler of this.handlers.get('close') || []) handler(); }
  querySelectorAll(tag) { return walk(this).filter(e => e !== this && e.tagName === tag); }
}
const walk = node => [node, ...node.children.flatMap(walk)];
function fixture(role = 'owner') {
  const elements = {'page-operations':new Element('main'), 'page-audit':new Element('main')};
  const body = new Element('body'); body.append(...Object.values(elements));
  const events = new Map(), timers = new Set(), requests = [], pending = new Map();
  let epoch = 1, authenticated = true;
  const context = {window:{addEventListener:(k,v)=>events.set(k,v)}, document:{body,hidden:false,createElement:t=>new Element(t),getElementById:id=>elements[id],addEventListener:(k,v)=>events.set(k,v)},
    navigator:{clipboard:{writeText:async()=>{}}},URLSearchParams,Date,
    setTimeout:callback=>{const t={callback};timers.add(t);return t;},clearTimeout:t=>timers.delete(t)};
  vm.runInNewContext(source,context);
  const request = async (path,method='GET',body) => {
    requests.push({path,method,body}); if(pending.has(path)) return pending.get(path).shift();
    if(path==='operations/health') return {status:'attention',checked_at:Date.now(),issues:[{code:'backup_due',title:'需要创建备份',level:'warning',detail:'请备份'}],backup:{}};
    if(path==='operations/members') return method==='GET'?{members:[]}:{key:'scm_'+'x'.repeat(43)};
    if(path.startsWith('operations/audit?')) return {records:[],before:0};
    throw Error('unexpected request '+path);
  };
  const api=context.window.SalcaraOperations({request,getEpoch:()=>epoch,isAuthenticated:()=>authenticated,getRole:()=>role});
  return {api,elements,body,events,timers,requests,pending,all:()=>walk(body),close(){authenticated=false;epoch++;api.reset();},find(tag,text){const found=walk(body).find(n=>n.tagName===tag&&n.textContent===text);assert.ok(found,`missing ${tag} ${text}`);return found;}};
}

test('operations is idle off-page, scoped polling stops when hidden or logged out',async()=>{
  const f=fixture();assert.equal(f.requests.length,0);f.api.setVisible('operations');await settle();
  assert.deepEqual(f.requests.map(r=>r.path),['operations/health','operations/members']);assert.equal(f.timers.size,1);
  f.api.setVisible('devices');assert.equal(f.timers.size,0);f.api.setVisible('operations');await settle();f.close();assert.equal(f.timers.size,0);
});
test('viewer sees health without requesting member privileges',async()=>{
  const f=fixture('viewer');f.api.setVisible('operations');await settle();assert.deepEqual(f.requests.map(r=>r.path),['operations/health']);
  const memberPanel=f.elements['page-operations'].children[2];assert.equal(memberPanel.hidden,true);
});
test('new independent secret is cleared when its dialog closes and never survives logout',async()=>{
  const f=fixture();f.api.setVisible('operations');await settle();const form=f.all().find(n=>n.tagName==='form'&&n.className==='ops-member-form');
  form.children[0].children[1].value='同事';form.children[1].children[1].value='viewer';await form.emit('submit');
  const secret=f.all().find(n=>n.attributes['aria-label']==='新管理密钥');assert.equal(secret.value.length,47);
  await f.find('button','已保存，关闭').emit('click');assert.equal(secret.value,'');
  form.children[0].children[1].value='同事 2';await form.emit('submit');assert.equal(secret.value.length,47);f.close();assert.equal(secret.value,'');
});
test('a late creation response cannot reveal a key in a closed session',async()=>{
  const f=fixture();f.api.setVisible('operations');await settle();const d=deferred();f.pending.set('operations/members',[d.promise]);
  const form=f.all().find(n=>n.tagName==='form'&&n.className==='ops-member-form');form.children[0].children[1].value='late';form.children[1].children[1].value='viewer';const sent=form.emit('submit');f.close();d.resolve({key:'scm_'+'z'.repeat(43)});await sent;
  const secret=f.all().find(n=>n.attributes['aria-label']==='新管理密钥');assert.equal(secret.value,'');assert.equal(f.all().filter(n=>n.tagName==='dialog').some(n=>n.open),false);
});
test('late health reads cannot replace a newer refresh or repopulate after logout',async()=>{
  const f=fixture();const old=deferred(),fresh=deferred();f.pending.set('operations/health',[old.promise,fresh.promise]);f.api.setVisible('operations');f.api.refresh();
  fresh.resolve({status:'healthy',checked_at:Date.now(),issues:[],backup:{}});await settle();old.resolve({status:'critical',checked_at:1,issues:[{title:'stale',detail:'stale'}],backup:{}});await settle();assert.equal(f.body.textContent.includes('stale'),false);
  const late=deferred();f.pending.set('operations/health',[late.promise]);f.api.refresh();f.close();late.resolve({status:'critical',checked_at:1,issues:[{title:'secret state'}],backup:{}});await settle();assert.equal(f.body.textContent.includes('secret state'),false);
});
