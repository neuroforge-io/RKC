package export

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserResultPagesRemainReachableBoundedAndSnapshotBound(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	bundle := exportFixture(t.TempDir(), "paging.go", []byte("package paging\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"list-pagination", "Repository result pages", "list-previous", "list-next", "aria-controls=\"list\""} {
		if !strings.Contains(string(assets["index.html"]), marker) {
			t.Fatalf("missing accessible page control %q", marker)
		}
	}
	application := strings.TrimSuffix(string(assets["app.js"]), "boot();")
	const prelude = `
const elements=new Map();let focused=0;
function element(id){
  if(!elements.has(id))elements.set(id,{value:'',innerHTML:'',textContent:'',hidden:false,disabled:false,attributes:{},
    setAttribute(name,value){this.attributes[name]=value},getAttribute(name){return this.attributes[name]},addEventListener(){},focus(){focused++},select(){},scrollIntoView(){},
    querySelectorAll(selector){return selector==='[role="option"]'?[{focus(){focused++}}]:[]},querySelector(){return null},classList:{toggle(){},add(){},remove(){}}});
  return elements.get(id);
}
global.document={getElementById:element,querySelectorAll(){return[]},addEventListener(){},body:{classList:{toggle(){}}}};
global.window={addEventListener(){}};global.location={hash:'',pathname:'/',search:''};global.history={replaceState(){}};
`
	const harness = `
function deferred(){let resolve;return{promise:new Promise(done=>resolve=done),resolve}}
function response(data,status=200,snapshot='page-snapshot'){return{ok:status===200,status,headers:{get(){return snapshot}},json:async()=>data}}
function nodes(start,count){return Array.from({length:count},(_,index)=>({id:'node-'+(start+index),name:'Entity '+String(start+index).padStart(5,'0'),kind:'function',language:'go'}))}
function reset(api=true){
  clearTimeout(state.searchTimer);elements.clear();focused=0;
  state.api=api;state.atlasRevision=1;state.navigationRevision=0;state.searchRevision=1;state.listPaging=null;state.staticPaging=null;state.staticPageIndex=0;state.staticResultCount=0;
  state.bundle={snapshot:{id:'page-snapshot',root_name:'Pages'},nodes:[],artifacts:[],edges:[],evidence:[],diagnostics:[]};state.coverage={};
  for(const map of [state.nodes,state.artifacts,state.evidence,state.outgoing,state.incoming,state.browseNodeIDs,state.hydratedNodeIDs])map.clear();
  state.selected=null;state.selectedArtifact=null;state.selectedArtifactContext=null;state.apiSearchResults=null;state.staticBootstrap=false;state.staticSearchRecords=null;state.staticSearchLoad=null;state.staticSearchByID=new Map();state.listTruncated=false;state.view='overview';
}
function firstBrowse(total=1320){
  const values=nodes(0,Math.min(120,total));
  installFirstListPage({snapshot_id:'page-snapshot',total,next_cursor:total>120?'offset-120':undefined,truncated:total>120},values,'/api/v1/nodes',new URLSearchParams({limit:'120'}),'');renderList();
}
function browseResponse(path,total=1320){
  const url=new URL(path,'http://local'),start=Number((url.searchParams.get('cursor')||'offset-0').slice(7)),limit=Number(url.searchParams.get('limit')),count=Math.min(limit,total-start),end=start+count;
  return response({snapshot_id:'page-snapshot',items:nodes(start,count),total,next_cursor:end<total?'offset-'+end:undefined,truncated:end<total});
}
function assert(condition,message){if(!condition)throw new Error(message)}
(async()=>{
  reset();firstBrowse();const paths=[],seen=new Set(state.results.map(item=>item.id));
  state.nodes.set('selected-detail',{id:'selected-detail',name:'Retained detail'});state.hydratedNodeIDs.add('selected-detail');
  global.fetch=async path=>{paths.push(path);return browseResponse(path)};
  while(state.listPaging.nextCursor){
    await changeListPage(1);for(const item of state.results)seen.add(item.id);
    assert(state.results.length<=200&&state.nodes.size<=201,'page traversal grew the visible window or unhydrated node cache');
    assert(state.nodes.has('selected-detail'),'page change discarded explicit detail data');
  }
  assert(seen.size===1320&&state.results[0].id==='node-1120'&&element('list-next').disabled,'forward traversal lost records or did not expose the terminal page');
  assert(state.listPaging.history.every(entry=>!Object.values(entry).some(Array.isArray)),'cursor history retained page row arrays');
  const pages=state.listPaging.history.length;
  while(state.listPaging.index)await changeListPage(-1);
  assert(state.results.length===120&&state.results[0].id==='node-0'&&element('list-previous').disabled,'previous page did not restore the original 120-row bootstrap boundary');
  assert(paths.length===(pages-1)*2&&new URL(paths.at(-1),'http://local').searchParams.get('limit')==='120','previous pages were cached or refetched with different limits');
  assert(element('result-summary').textContent.includes('1–120 of 1,320'),'visible range/total is not explicit');
  assert(element('list-next').textContent==='Next page','already visited forward page was mislabeled as new records');

  reset();firstBrowse(320);const one=deferred();let calls=0;global.fetch=()=>{calls++;return one.promise};
  const pending=changeListPage(1);await changeListPage(1);await changeListPage(-1);
  assert(calls===1&&element('list-next').disabled&&element('list').attributes['aria-busy']==='true','concurrent page clicks dispatched more than one read');
  setView('coverage',false);const content=element('content').innerHTML;focused=0;
  one.resolve(browseResponse('/api/v1/nodes?cursor=offset-120&limit=200',320));await pending;
  assert(state.listPaging.index===1&&element('content').innerHTML===content&&focused===0,'a valid page response either vanished on tab change or stole content focus');

  reset();firstBrowse(320);const before=element('list').innerHTML;let attempts=0;
  global.fetch=async path=>++attempts===1?response({detail:'temporary page failure'},500):browseResponse(path,320);
  await changeListPage(1);
  assert(state.listPaging.index===0&&element('list').innerHTML===before&&element('list-page-status').textContent.includes('temporary page failure')&&!element('list-next').disabled,'failed page read replaced current results or prevented retry');
  await changeListPage(1);assert(state.listPaging.index===1&&attempts===2,'retry did not resume the same cursor');

  for(const staleError of [false,true]){
    reset();firstBrowse(320);const obsolete=deferred();global.fetch=()=>obsolete.promise;
    const oldPage=changeListPage(1);element('search').value='replacement';scheduleListRefresh();clearTimeout(state.searchTimer);
    global.fetch=async()=>response({snapshot_id:'page-snapshot',total:1,hits:[{document:{id:'fresh',object_type:'node',kind:'function',title:'Fresh result'},score:9}]});
    await refreshAPIList(state.searchRevision);const fresh=element('list').innerHTML;
    obsolete.resolve(staleError?response({detail:'obsolete page error'},500):browseResponse('/api/v1/nodes?cursor=offset-120&limit=200',320));await oldPage;
    assert(element('list').innerHTML===fresh&&state.results[0].id==='fresh'&&!element('list-page-status').textContent.includes('obsolete'),'stale page result/error disturbed newer search results');
  }
  reset();firstBrowse(320);const oldAtlas=deferred();global.fetch=()=>oldAtlas.promise;
  const oldAtlasPage=changeListPage(1);advanceAtlasGeneration();state.bundle.snapshot.id='new-snapshot';state.nodes.clear();element('list').innerHTML='new atlas rows';
  oldAtlas.resolve(browseResponse('/api/v1/nodes?cursor=offset-120&limit=200',320));await oldAtlasPage;
  assert(state.listPaging===null&&state.nodes.size===0&&element('list').innerHTML==='new atlas rows','page continuation crossed snapshot activation');

  reset();firstBrowse(320);global.fetch=async path=>{const data=await browseResponse(path,320).json();data.snapshot_id='wrong-snapshot';return response(data)};
  await changeListPage(1);assert(state.listPaging.index===0&&state.listPaging.error.includes('snapshot'),'page body identity mismatch was accepted');
  global.fetch=async()=>response({snapshot_id:'page-snapshot',items:nodes(120,201),total:321});
  await changeListPage(1);assert(state.results.length===120&&state.listPaging.error.includes('row window'),'oversized page silently dropped records instead of failing');

  for(const invalid of [
    {next_cursor:'another-page',truncated:false,total:520},
    {next_cursor:'é'.repeat(3000),truncated:true,total:520},
    {truncated:true,total:520},
    {next_cursor:'offset-120',truncated:true,total:520},
    {truncated:false,total:520},
  ]){
    reset();firstBrowse(520);global.fetch=async()=>response({snapshot_id:'page-snapshot',items:nodes(120,200),...invalid});
    await changeListPage(1);assert(state.listPaging.index===0&&state.listPaging.error&&state.results.length===120,'invalid continuation metadata was accepted');
  }
  reset();installFirstListPage({total:320,truncated:true},nodes(0,120),'/api/v1/nodes',new URLSearchParams({limit:'120'}),'');renderList();
  assert(element('list-page-status').textContent.includes('did not provide a continuation cursor'),'legacy truncated result lost its honest continuation limitation');

  reset();element('search').value='ranked topic';element('kind').value='function';element('language').value='go';const searchPaths=[];
  global.fetch=async path=>{
    searchPaths.push(path);const parameters=new URL(path,'http://local').searchParams,start=parameters.has('cursor')?50:0;
    return response({snapshot_id:'page-snapshot',total:100,next_cursor:start===0?'ranked-next':undefined,truncated:start===0,
      hits:Array.from({length:50},(_,index)=>({document:{id:'rank-'+(start+index),object_type:index%2?'artifact':'node',title:'Title '+(100-start-index),kind:'function',language:'go'},score:100-start-index}))});
  };
  await refreshAPIList(state.searchRevision);await changeListPage(1);
  assert(state.results[0].id==='rank-50'&&state.results.at(-1).id==='rank-99'&&state.results[1].objectType==='artifact','ranked heterogeneous search order changed on continuation');
  for(const path of searchPaths){const parameters=new URL(path,'http://local').searchParams;assert(parameters.get('q')==='ranked topic'&&parameters.get('kinds')==='function'&&parameters.get('languages')==='go'&&parameters.get('object_types')==='node,artifact'&&parameters.get('limit')==='50','search page lost its exact query/filter binding')}
  await changeListPage(-1);assert(state.results[0].id==='rank-0'&&searchPaths.length===3,'ranked previous page was not refetched');

  reset(false);state.bundle.nodes=nodes(0,1250);for(const node of state.bundle.nodes)state.nodes.set(node.id,node);renderList();const offline=new Set();
  do{for(const item of state.results)offline.add(item.id);assert(state.results.length<=200,'offline page exceeded visible row window');if(element('list-next').disabled)break;await changeListPage(1)}while(true);
  assert(offline.size===1250,'offline rows beyond the original 1000 cap became unreachable');
  await changeListPage(-1);assert(state.results[0].id==='node-1000','offline previous page lost its range');

  function offlineBootstrap(){reset(false);state.staticBootstrap=true;state.listTruncated=true;state.coverage.nodes_total=1250;state.bundle.nodes=nodes(0,120);for(const node of state.bundle.nodes)state.nodes.set(node.id,node);renderList()}
  const compact={schema_version:'1',snapshot_id:'page-snapshot',node_count:1250,records:nodes(0,1250).map(node=>({...node,search_text:node.name.toLowerCase()+' function go'}))};
  offlineBootstrap();const compactPaths=[];let compactAttempt=0;
  global.fetch=async path=>{compactPaths.push(path);return ++compactAttempt===1?response({},500):response(compact)};
  assert(!element('list-pagination').hidden&&element('list-next').textContent==='Load full list'&&element('result-summary').textContent.includes('of 1,250'),'offline bootstrap concealed entities beyond its first120');
  await changeListPage(1);
  assert(state.results.length===120&&element('list-page-status').textContent.includes('Offline list could not be opened')&&!element('list-next').disabled,'failed compact-index load replaced bootstrap rows or prevented retry');
  await changeListPage(1);
  assert(state.results.length===200&&state.staticPageIndex===0&&element('list-page-status').textContent.includes('first page')&&state.nodes.size===120,'compact index did not open an explicit bounded first page');
  const allOffline=new Set();
  do{for(const item of state.results)allOffline.add(item.id);if(element('list-next').disabled)break;await changeListPage(1)}while(true);
  assert(allOffline.size===1250&&compactPaths.length===2&&compactPaths.every(path=>path==='./data/search.json'),'offline initial browse fetched full atlas details or left later records unreachable');

  offlineBootstrap();const loadingCompact=deferred();let compactReads=0;global.fetch=()=>{compactReads++;return loadingCompact.promise};
  const openingCompact=changeListPage(1);await changeListPage(1);
  assert(compactReads===1&&element('list-next').disabled,'duplicate compact-index click started another request');
  element('search').value='00001';scheduleListRefresh();loadingCompact.resolve(response(compact));await openingCompact;await new Promise(resolve=>setTimeout(resolve,210));
  assert(state.results.length===1&&state.results[0].id==='node-1'&&!element('list-page-status').textContent.includes('could not'),'completed compact-index load ignored newer search intent');

  offlineBootstrap();const obsoleteCompact=deferred();global.fetch=()=>obsoleteCompact.promise;
  const oldCompact=changeListPage(1);advanceAtlasGeneration();state.bundle.snapshot.id='new-offline-snapshot';element('list').innerHTML='new offline rows';
  obsoleteCompact.resolve(response(compact));await oldCompact;
  assert(state.staticSearchRecords===null&&element('list').innerHTML==='new offline rows','old compact index crossed snapshot activation');
  clearTimeout(state.searchTimer);console.log('result-paging-ok');
})().catch(error=>{clearTimeout(state.searchTimer);console.error(error.stack);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + application + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser paging adversary failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "result-paging-ok") {
		t.Fatalf("browser paging adversary did not finish: %s", output)
	}
}
