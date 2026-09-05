package export

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserNavigationKeepsLatestUserIntent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	bundle := exportFixture(t.TempDir(), "navigation.go", []byte("package navigation\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := strings.TrimSuffix(string(assets["app.js"]), "boot();")
	const prelude = `
const elements=new Map();
function element(id){
  if(!elements.has(id))elements.set(id,{value:'',innerHTML:'',textContent:'',hidden:false,disabled:false,
    setAttribute(){},getAttribute(){return''},addEventListener(){},focus(){},select(){},scrollIntoView(){},
    querySelectorAll(){return[]},querySelector(){return null},classList:{toggle(){},add(){},remove(){}}});
  return elements.get(id);
}
global.document={getElementById:element,querySelectorAll(){return[]},addEventListener(){},body:{classList:{toggle(){}}}};
global.window={addEventListener(){}};let fragment='';
global.location={pathname:'/',search:'',get hash(){return fragment},set hash(value){fragment=value&&!value.startsWith('#')?'#'+value:value}};
global.history={replaceState(){location.hash=''}};
global.sessionStorage={getItem(){return null},setItem(){},removeItem(){}};
`
	const harness = `
function deferred(){let resolve;return{promise:new Promise(done=>resolve=done),resolve}}
function response(data,status=200){return{ok:status===200,status,headers:{get(){return 'navigation-snapshot'}},json:async()=>data}}
function detail(id){return{node:{id,name:id,kind:'function'},evidence:[],outgoing_edges:[],incoming_edges:[]}}
function artifact(id){return{artifact:{id,path:id+'.txt',kind:'file'},nodes:[]}}
function reset(api=true){
  clearTimeout(state.toastTimer);elements.clear();location.hash='';
  state.api=api;state.atlasRevision=1;state.navigationRevision=0;state.searchRevision=0;
  state.bundle={snapshot:{id:'navigation-snapshot',root_name:'Navigation'},nodes:[],artifacts:[],edges:[],evidence:[],diagnostics:[]};
  state.coverage={};state.nodes.clear();state.artifacts.clear();state.evidence.clear();state.outgoing.clear();state.incoming.clear();
  state.selected=null;state.selectedArtifact=null;state.selectedArtifactContext=null;state.apiSearchResults=null;
  state.staticBootstrap=false;state.staticLoad=null;state.view='overview';state.workbench=null;
  state.capabilities={schema_version:'rkc-capabilities/v1'};
}
const settle=()=>new Promise(resolve=>setImmediate(resolve));
function assert(condition,message){if(!condition)throw new Error(message)}
(async()=>{
  reset();state.nodes.set('A',detail('A').node);
  const pendingA=deferred();global.fetch=()=>pendingA.promise;
  const selectingA=selectNode('A','symbol',false);
  assert(state.view==='symbol'&&element('content').innerHTML.includes('Loading repository entity'),'selection did not show its pending state');
  setView('coverage',false);const coverage=element('content').innerHTML;
  pendingA.resolve(response(detail('A')));await selectingA;
  assert(state.view==='coverage'&&element('content').innerHTML===coverage&&location.hash==='','late node detail stole the tab or changed the URL');

  reset();const slowA=deferred(),counts={};
  global.fetch=async path=>{const id=path.split('/').pop();counts[id]=(counts[id]||0)+1;return id==='A'?slowA.promise:response(detail(id))};
  const firstNode=selectNode('A','symbol',false);await selectNode('B','symbol',false);
  const visibleB=element('content').innerHTML;
  slowA.resolve(response(detail('A')));await firstNode;
  assert(state.selected==='B'&&element('content').innerHTML===visibleB&&location.hash==='#B','out-of-order node detail replaced the latest selection');
  assert(counts.A===1&&counts.B===1,'an uncached selection fetched node detail more than once');

  reset();const slowArtifact=deferred();
  global.fetch=path=>path.endsWith('/A')?slowArtifact.promise:Promise.resolve(response(artifact('B')));
  const firstArtifact=selectArtifactSearchResult({id:'A'},false);await selectArtifactSearchResult({id:'B'},false);
  const artifactB=element('content').innerHTML;
  slowArtifact.resolve(response(artifact('A')));await firstArtifact;
  assert(state.selectedArtifact==='B'&&element('content').innerHTML===artifactB,'out-of-order artifact detail replaced the latest selection');

  reset();const slowNode=deferred();
  global.fetch=path=>path.includes('/nodes/')?slowNode.promise:Promise.resolve(response(artifact('B')));
  const oldNode=selectNode('A','symbol',false);await selectArtifactSearchResult({id:'B'},false);
  slowNode.resolve(response(detail('A')));await oldNode;
  assert(state.selected===null&&state.selectedArtifact==='B'&&element('content').innerHTML.includes('B.txt'),'late node detail replaced a newer artifact selection');

  for(const objectType of ['node','artifact']){
    reset();const failed=deferred();global.fetch=()=>failed.promise;
    const action=runUIAction(()=>objectType==='node'?selectNode('A','symbol',false):selectArtifactSearchResult({id:'A'},false));
    setView('coverage',false);const visible=element('content').innerHTML;
    failed.resolve(response({detail:'obsolete '+objectType+' failure'},500));await action;
    assert(state.view==='coverage'&&element('content').innerHTML===visible&&!element('toast').textContent,'stale '+objectType+' error changed the current view or raised a toast');
  }
  reset();global.fetch=async()=>response({detail:'current failure'},500);
  await runUIAction(()=>selectNode('A','symbol',false));
  assert(element('content').innerHTML.includes('Details failed to load')&&element('toast').textContent.includes('current failure'),'current selection failure was concealed');

  for(const fails of [false,true]){
    reset(false);state.staticBootstrap=true;const full=deferred();global.fetch=()=>full.promise;
    setView('diagnostics',false);const loading=state.staticLoad;
    assert(state.view==='diagnostics','offline loading did not select the requested tab');
    setView('coverage',false);const visible=element('content').innerHTML;
    full.resolve(fails?response({},500):response({bundle:state.bundle,coverage:{}}));
    await loading.catch(()=>{});await settle();
    assert(state.view==='coverage'&&element('content').innerHTML===visible,'offline '+(fails?'failure':'completion')+' replaced a newer tab');
  }
  reset(false);state.staticBootstrap=true;const full=deferred();let fullReads=0;
  global.fetch=()=>{fullReads++;return full.promise};
  const staticA=selectNode('A','symbol',false),staticB=selectNode('B','symbol',false);
  full.resolve(response({bundle:{...state.bundle,nodes:[detail('A').node,detail('B').node]},coverage:{}}));
  await Promise.all([staticA,staticB]);
  assert(state.selected==='B'&&location.hash==='#B'&&element('content').innerHTML.includes('<h2>B</h2>')&&fullReads===1,'shared offline load did not retain the latest entity selection');

  for(const fails of [false,true]){
    reset();state.nodes.set('seed',detail('seed').node);state.selected='seed';
    const oldGraph=deferred();let graphReads=0;
    global.fetch=()=>++graphReads===1?oldGraph.promise:Promise.resolve(response({nodes:[detail('seed').node],edges:[]}));
    setView('graph',false);setView('coverage',false);setView('graph',false);await settle();
    const currentGraph=element('content').innerHTML;
    oldGraph.resolve(fails?response({detail:'obsolete graph failure'},500):response({nodes:[detail('stale-neighbor').node],edges:[]}));await settle();
    assert(state.view==='graph'&&element('content').innerHTML===currentGraph&&!state.nodes.has('stale-neighbor'),'older graph request affected a newer visit to the same graph');
  }

  reset();const oldSnapshot=deferred();global.fetch=()=>oldSnapshot.promise;
  const staleAction=runUIAction(()=>selectNode('A','symbol',false));
  advanceAtlasGeneration();state.bundle={...state.bundle,snapshot:{id:'new-snapshot'}};state.selected=null;setView('coverage',false);
  const currentAtlas=element('content').innerHTML;
  oldSnapshot.resolve(response(detail('A')));await staleAction;
  assert(!state.nodes.has('A')&&element('content').innerHTML===currentAtlas&&!element('toast').textContent,'stale snapshot detail crossed activation or raised an obsolete error');

  reset();location.hash='deep-link';let detailReads=0;
  global.fetch=async path=>{
    if(path==='/api/v1/health')return response({snapshot_id:'navigation-snapshot'});
    if(path==='/api/v1/manifest')return response(state.bundle.snapshot);
    if(path==='/api/v1/coverage')return response({snapshot_id:'navigation-snapshot'});
    if(path==='/api/v1/facets')return response({});
    if(path==='/api/v1/workbench/session')return response({},403);
    if(path==='/api/v1/nodes/deep-link'){detailReads++;return response(detail('deep-link'))}
    if(path.startsWith('/api/v1/nodes?')||path.startsWith('/api/v1/diagnostics?'))return response({items:[]});
    throw new Error('unexpected boot request '+path);
  };
  await boot();
  assert(state.selected==='deep-link'&&state.view==='symbol'&&detailReads===1,'deep-link bootstrap did not load its node exactly once: '+element('content').innerHTML);

  reset();const session=deferred(),readyForSession=deferred(),bootFetch=global.fetch;
  global.fetch=path=>{if(path==='/api/v1/workbench/session'){readyForSession.resolve();return session.promise}return bootFetch(path)};
  const initialBoot=boot();await readyForSession.promise;setView('coverage',false);const duringBoot=element('content').innerHTML;
  session.resolve(response({},403));await initialBoot;
  assert(state.view==='coverage'&&element('content').innerHTML===duringBoot,'late session discovery overrode navigation during startup');

  reset();location.hash='deep-link';const bootDetail=deferred(),readyForDetail=deferred();
  global.fetch=path=>{if(path==='/api/v1/nodes/deep-link'){readyForDetail.resolve();return bootDetail.promise}return bootFetch(path)};
  const bootWithSelection=boot();await readyForDetail.promise;setView('coverage',false);const duringSelection=element('content').innerHTML;
  bootDetail.resolve(response({detail:'obsolete deep-link failure'},500));await bootWithSelection;
  assert(state.view==='coverage'&&element('content').innerHTML===duringSelection,'stale deep-link failure replaced navigation during startup');
  clearTimeout(state.toastTimer);console.log('navigation-intent-ok');
})().catch(error=>{clearTimeout(state.toastTimer);console.error(error.stack);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + application + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser navigation adversary failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "navigation-intent-ok") {
		t.Fatalf("browser navigation adversary did not finish: %s", output)
	}
}
