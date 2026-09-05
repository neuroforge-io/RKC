package export

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserDiagnosticPagesFiltersAndFailuresStayBounded(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	bundle := exportFixture(t.TempDir(), "diagnostics.go", []byte("package diagnostics\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := strings.TrimSuffix(string(assets["app.js"]), "boot();")
	const prelude = `
const elements=new Map();let focused=0;
function element(id){if(!elements.has(id))elements.set(id,{value:'',innerHTML:'',textContent:'',hidden:false,disabled:false,attributes:{},setAttribute(name,value){this.attributes[name]=value},getAttribute(name){return this.attributes[name]},addEventListener(){},focus(){focused++},select(){},scrollIntoView(){},querySelectorAll(){return[]},querySelector(){return null},classList:{toggle(){},add(){},remove(){}}});return elements.get(id)}
global.document={getElementById:element,querySelectorAll(){return[]},body:{classList:{toggle(){}}}};
global.window={addEventListener(){}};global.location={hash:'',pathname:'/',search:''};global.history={replaceState(){}};
`
	const harness = `
const records=Array.from({length:1105},(_,index)=>({id:'diagnostic-'+index,severity:index%2?'warning':'error',code:'CODE_'+index%3,message:index===0?'<script>untrusted()</script>':'Diagnostic '+index,stage:'parse',source:{path:'example.go',start_line:index+1}}));
function response(data,status=200){return{ok:status===200,status,headers:{get(){return 'diagnostic-snapshot'}},json:async()=>data}}
function deferred(){let resolve;return{promise:new Promise(done=>resolve=done),resolve}}
function reset(api=true){
  elements.clear();focused=0;state.api=api;state.atlasRevision=1;state.navigationRevision=0;resetDiagnosticPaging();
  state.bundle={snapshot:{id:'diagnostic-snapshot',root_name:'Diagnostics'},nodes:[],artifacts:[],edges:[],evidence:[],diagnostics:[]};state.coverage={diagnostics_by_severity:{warning:552,error:553}};
  state.facets=null;state.nodes.clear();state.artifacts.clear();state.evidence.clear();state.outgoing.clear();state.incoming.clear();state.staticBootstrap=false;state.staticLoad=null;state.view='diagnostics';
}
function pageResponse(path){
  const parameters=new URL(path,'http://local').searchParams,matching=records.filter(item=>(!parameters.get('severity')||item.severity===parameters.get('severity'))&&(!parameters.get('code')||item.code===parameters.get('code'))),start=Number(parameters.get('cursor')||0),limit=Number(parameters.get('limit')||200),end=Math.min(matching.length,start+limit);
  return response({snapshot_id:'diagnostic-snapshot',items:matching.slice(start,end),total:matching.length,truncated:end<matching.length,next_cursor:end<matching.length?String(end):undefined});
}
function firstPage(){installFirstDiagnosticPage({snapshot_id:'diagnostic-snapshot',total:records.length,truncated:true,next_cursor:'200'},records.slice(0,200),{severity:'',code:''});renderDiagnostics()}
function assert(condition,message){if(!condition)throw new Error(message)}
(async()=>{
  reset();firstPage();const reads=[],seen=new Set(state.bundle.diagnostics.map(item=>item.id));global.fetch=async path=>{reads.push(path);return pageResponse(path)};
  assert(element('content').innerHTML.includes('aria-label="Diagnostic pages"')&&element('content').innerHTML.includes('aria-controls="diagnostic-list"'),'diagnostic page controls are not labeled and linked');
  assert(!element('content').innerHTML.includes('<script>')&&element('content').innerHTML.includes('&lt;script&gt;'),'diagnostic text was interpreted as markup');
  while(state.diagnosticPaging.nextCursor){await changeDiagnosticPage(1);for(const item of state.bundle.diagnostics)seen.add(item.id);assert(state.bundle.diagnostics.length<=200,'diagnostics cache grew beyond current page')}
  assert(seen.size===1105&&element('diagnostic-next').disabled,'diagnostics beyond initial200 were not reachable');
  assert(state.diagnosticPaging.history.every(entry=>!Object.values(entry).some(Array.isArray)),'diagnostic cursor history retained row arrays');
  while(state.diagnosticPaging.index)await changeDiagnosticPage(-1);
  assert(state.bundle.diagnostics[0].id==='diagnostic-0'&&state.bundle.diagnostics.length===200&&reads.length===10,'diagnostic previous page was not refetched with its original limit');

  element('diagnostic-severity').value='warning';await applyDiagnosticFilters();
  assert(state.diagnosticFilters.severity==='warning'&&state.diagnosticPaging.total===552&&state.bundle.diagnostics.every(item=>item.severity==='warning'),'severity filter did not bind the first page');
  await changeDiagnosticPage(1);assert(reads.at(-1).includes('severity=warning')&&state.bundle.diagnostics.every(item=>item.severity==='warning'),'severity filter was lost on continuation');
  element('diagnostic-code').value='CODE_2';await applyDiagnosticFilters();
  assert(state.bundle.diagnostics.every(item=>item.severity==='warning'&&item.code==='CODE_2')&&reads.at(-1).includes('code=CODE_2'),'exact diagnostic-code filter was lost');
  element('diagnostic-code').value='MISSING';await applyDiagnosticFilters();assert(state.bundle.diagnostics.length===0&&element('content').innerHTML.includes('No diagnostics match these filters.'),'filtered empty state claimed snapshot had no diagnostics');

  reset();firstPage();const firstIDs=state.bundle.diagnostics.map(item=>item.id).join(',');let attempts=0;global.fetch=async path=>++attempts===1?response({detail:'temporary diagnostic failure'},500):pageResponse(path);
  await changeDiagnosticPage(1);assert(state.diagnosticPaging.index===0&&state.bundle.diagnostics.map(item=>item.id).join(',')===firstIDs&&state.diagnosticRequest.error.includes('temporary diagnostic failure')&&!element('diagnostic-next').disabled,'failed diagnostic read replaced current page or blocked retry');
  await changeDiagnosticPage(1);assert(state.diagnosticPaging.index===1&&attempts===2,'diagnostic retry failed to resume the page');

  reset();firstPage();const slow=deferred();let duplicateReads=0;global.fetch=()=>{duplicateReads++;return slow.promise};
  const pending=changeDiagnosticPage(1);await changeDiagnosticPage(1);assert(duplicateReads===1&&element('diagnostic-list').attributes['aria-busy']==='true','duplicate diagnostic page click started another request');
  setView('coverage',false);const coverage=element('content').innerHTML;focused=0;slow.resolve(pageResponse('/api/v1/diagnostics?cursor=200&limit=200'));await pending;
  assert(state.diagnosticPaging.index===1&&element('content').innerHTML===coverage&&focused===0,'diagnostic completion replaced newer content navigation');

  for(const error of [false,true]){
    reset();firstPage();const old=deferred();global.fetch=()=>old.promise;const oldPage=changeDiagnosticPage(1);
    element('diagnostic-severity').value='error';global.fetch=async path=>pageResponse(path);await applyDiagnosticFilters();const fresh=element('content').innerHTML;
    old.resolve(error?response({detail:'obsolete diagnostic error'},500):pageResponse('/api/v1/diagnostics?cursor=200&limit=200'));await oldPage;
    assert(state.diagnosticFilters.severity==='error'&&state.bundle.diagnostics.every(item=>item.severity==='error')&&element('content').innerHTML===fresh&&!state.diagnosticRequest.error,'stale diagnostic page or error disturbed a newer filter');
  }
  reset();firstPage();const oldFilter=deferred();let filterReads=0;global.fetch=()=>{filterReads++;return oldFilter.promise};element('diagnostic-severity').value='warning';const filtering=applyDiagnosticFilters();await applyDiagnosticFilters();assert(filterReads===1,'duplicate filter submission dispatched another request');
  element('diagnostic-severity').value='error';global.fetch=async path=>pageResponse(path);await applyDiagnosticFilters();oldFilter.resolve(response({detail:'obsolete filter error'},500));await filtering;
  assert(state.diagnosticFilters.severity==='error'&&!state.diagnosticRequest.error,'late filter failure overwrote the latest filter');

  reset();firstPage();global.fetch=async()=>response({detail:'filter unavailable'},500);element('diagnostic-code').value='CODE_1';await applyDiagnosticFilters();
  assert(state.diagnosticFilters.code===''&&state.diagnosticDraft.code==='CODE_1'&&state.bundle.diagnostics.length===200&&state.diagnosticRequest.error.includes('Apply filters to retry'),'filter failure lost current records or the retry draft');
  global.fetch=async path=>pageResponse(path);await applyDiagnosticFilters();assert(state.diagnosticFilters.code==='CODE_1','filter retry lost its requested code');

  reset();firstPage();const oldAtlas=deferred();global.fetch=()=>oldAtlas.promise;const oldPage=changeDiagnosticPage(1);advanceAtlasGeneration();state.bundle.snapshot.id='new-diagnostics';state.bundle.diagnostics=[];element('content').innerHTML='new snapshot visible';
  oldAtlas.resolve(pageResponse('/api/v1/diagnostics?cursor=200&limit=200'));await oldPage;
  assert(state.diagnosticPaging===null&&state.bundle.diagnostics.length===0&&element('content').innerHTML==='new snapshot visible','diagnostic page crossed atlas activation');
  reset();firstPage();global.fetch=async path=>{const data=await pageResponse(path).json();data.snapshot_id='wrong-diagnostics';return response(data)};await changeDiagnosticPage(1);
  assert(state.diagnosticPaging.index===0&&state.diagnosticRequest.error.includes('snapshot'),'diagnostic response body identity was not checked');

  reset(false);state.staticBootstrap=true;const full={bundle:{...state.bundle,diagnostics:records},coverage:state.coverage};const offlineReads=[];global.fetch=async path=>{offlineReads.push(path);return response(full)};
  setView('diagnostics',false);await state.staticLoad;await new Promise(resolve=>setImmediate(resolve));
  assert(!state.staticBootstrap&&state.bundle.diagnostics.length===1105&&(element('content').innerHTML.match(/role="listitem"/g)||[]).length===200,'offline diagnostics did not load complete data into bounded visible pages');
  const allOffline=new Set();
  for(;;){const start=state.diagnosticPageIndex*200;for(const item of records.slice(start,start+200))allOffline.add(item.id);if(element('diagnostic-next').disabled)break;await changeDiagnosticPage(1);assert((element('content').innerHTML.match(/role="listitem"/g)||[]).length<=200,'offline diagnostic page exceeded200 rows')}
  assert(allOffline.size===1105&&offlineReads.length===1&&offlineReads[0]==='./data/atlas.json','offline diagnostics were dropped or repeatedly loaded full atlas');
  element('diagnostic-severity').value='warning';element('diagnostic-code').value='CODE_1';await applyDiagnosticFilters();
  assert(state.diagnosticPageIndex===0&&state.diagnosticResultCount===records.filter(item=>item.severity==='warning'&&item.code==='CODE_1').length,'offline filters failed to reset page and match exact diagnostics');
  console.log('diagnostic-paging-ok');
})().catch(error=>{console.error(error.stack);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + application + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser diagnostics adversary failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "diagnostic-paging-ok") {
		t.Fatalf("browser diagnostics adversary did not finish: %s", output)
	}
}
