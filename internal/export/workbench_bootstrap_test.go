package export

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserAssetsConsumeWorkbenchBootstrapWithoutPersistentURLSecrets(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "bootstrap.go", []byte("package bootstrap\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"takeWorkbenchBootstrap",
		"new URLSearchParams(fragment)",
		"values.get('rkc-workbench')",
		"values.delete('rkc-workbench')",
		"history.replaceState",
		"sessionStorage.getItem('rkc-workbench-token')",
		"sessionStorage.setItem('rkc-workbench-token',token)",
		"sessionStorage.removeItem('rkc-workbench-token')",
		"authority_notice",
		"not a security sandbox",
		"headers['X-RKC-Workbench-Bootstrap']=bootstrap",
		"headers['X-RKC-Workbench-Token']=stored",
		"fetch('/api/v1/workbench/session',{cache:'no-store',headers})",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser application is missing protected-bootstrap marker %q", marker)
		}
	}
	removeFragment := strings.Index(application, "history.replaceState")
	exchangeSession := strings.Index(application, "fetch('/api/v1/workbench/session'")
	if removeFragment < 0 || exchangeSession < 0 || removeFragment >= exchangeSession {
		t.Fatalf("bootstrap URL cleanup must precede session exchange: cleanup=%d exchange=%d", removeFragment, exchangeSession)
	}
	if strings.Contains(application, "localStorage") ||
		strings.Contains(application, "sessionStorage.setItem('rkc-workbench-bootstrap'") ||
		strings.Contains(application, "?rkc-workbench=") {
		t.Fatal("browser application persists or transmits the bootstrap capability through an unsafe channel")
	}
}

func TestBrowserWorkbenchApplicationHasValidJavaScriptWhenNodeIsAvailable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; generated asset contract tests remain active")
	}
	bundle := exportFixture(t.TempDir(), "syntax.go", []byte("package syntax\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "--check", "-")
	command.Stdin = bytes.NewReader(assets["app.js"])
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated workbench JavaScript is invalid: %v\n%s", err, output)
	}
}

func TestBrowserRejectsMixedSnapshotGenerations(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "generation.go", []byte("package generation\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"snapshotGenerationHeader='X-RKC-Snapshot-ID'",
		"maximumSnapshotLoadAttempts=3",
		"responses.some(result=>result.snapshotID!==snapshotID)",
		"coverage?.snapshot_id!==snapshotID",
		"health?.snapshot_id!==snapshotID",
		"if(isSnapshotGenerationError(apiError))throw apiError",
		"Snapshot generation changed while the atlas was loading",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser snapshot-generation guard is missing %q", marker)
		}
	}
}

func TestBrowserBindsRepositoryReadsToOneActiveAtlasRevision(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "read-generation.go", []byte("package readgeneration\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"atlasRevision:0",
		"function advanceAtlasGeneration(){state.atlasRevision++;state.navigationRevision++;state.searchRevision++;state.listPaging=null;state.staticPaging=null;state.staticPageIndex=0;clearTimeout(state.searchTimer);return state.atlasRevision}",
		"const atlasRevision=state.atlasRevision,expectedSnapshot=state.bundle?.snapshot?.id",
		"response.headers?.get(snapshotGenerationHeader)",
		"responseSnapshot!==expectedSnapshot",
		"atlasRevision!==state.atlasRevision||state.bundle?.snapshot?.id!==expectedSnapshot",
		"const atlasRevision=advanceAtlasGeneration();",
		"applyAtlasData(data,atlasRevision)",
		"if(atlasRevision!==state.atlasRevision)throw error",
		"if(atlasRevision!==state.atlasRevision)return",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser active-atlas read guard is missing %q", marker)
		}
	}
	for _, route := range []string{
		"fetchJSON('/api/v1/nodes?'",
		"fetchJSON('/api/v1/nodes/'",
		"fetchJSON('/api/v1/graph/neighborhood?node_id='",
	} {
		if !strings.Contains(application, route) {
			t.Errorf("browser repository read does not use the guarded fetch path: %q", route)
		}
	}
	nodeStart := strings.Index(application, "async function loadAPINode(id){")
	nodeEnd := strings.Index(application, "function renderList(){")
	if nodeStart < 0 || nodeEnd <= nodeStart {
		t.Fatal("browser API node loader is missing")
	}
	nodeLoader := application[nodeStart:nodeEnd]
	nodeAwait := strings.Index(nodeLoader, "const detail=await fetchJSON(")
	nodeRevisionCheck := strings.Index(nodeLoader, "if(atlasRevision!==state.atlasRevision)throw snapshotGenerationError(")
	nodeMutation := strings.Index(nodeLoader, "state.nodes.set(")
	if !strings.Contains(nodeLoader, "const atlasRevision=state.atlasRevision") || nodeAwait < 0 || nodeRevisionCheck <= nodeAwait || nodeMutation <= nodeRevisionCheck {
		t.Fatalf("node loader does not recheck the caller-captured atlas revision before mutation: %s", nodeLoader)
	}
	graphStart := strings.Index(application, "async function renderGraph(seedID){")
	graphEnd := strings.Index(application, "function pushUnique(")
	if graphStart < 0 || graphEnd <= graphStart {
		t.Fatal("browser graph loader is missing")
	}
	graphLoader := application[graphStart:graphEnd]
	graphAwait := strings.Index(graphLoader, "const neighborhood=await fetchJSON(")
	graphRevisionCheck := strings.Index(graphLoader, "if(atlasRevision!==state.atlasRevision)return")
	graphMutation := strings.Index(graphLoader, "state.nodes.set(")
	if !strings.Contains(graphLoader, "const atlasRevision=state.atlasRevision") || graphAwait < 0 || graphRevisionCheck <= graphAwait || graphMutation <= graphRevisionCheck {
		t.Fatalf("graph loader does not recheck the caller-captured atlas revision before mutation: %s", graphLoader)
	}
}

func TestBrowserUsesFullSearchAPIAndNavigableArtifactResults(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "artifact-search.go", []byte("package artifactsearch\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"fetchJSON('/api/v1/search?'",
		"parameters.set('object_types','node,artifact')",
		"normaliseAPISearchHit",
		"boundedSearchExcerpt",
		"data-object-type=",
		"selectSearchResult",
		"selectArtifactSearchResult",
		"fetchJSON('/api/v1/artifacts/'",
		"Artifact detail completed after the active atlas generation changed",
		"function renderArtifact(id)",
		"Matched repository text",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser full-search/artifact contract is missing %q", marker)
		}
	}
	refreshStart := strings.Index(application, "async function refreshAPIList(revision){")
	refreshEnd := strings.Index(application, "function normaliseAPISearchHit(")
	if refreshStart < 0 || refreshEnd <= refreshStart {
		t.Fatal("browser API refresh flow is missing")
	}
	refresh := application[refreshStart:refreshEnd]
	searchRoute := strings.Index(refresh, "fetchJSON('/api/v1/search?'")
	nodeRoute := strings.Index(refresh, "fetchJSON('/api/v1/nodes?'")
	if searchRoute < 0 || nodeRoute <= searchRoute || !strings.Contains(refresh, "if(query){") {
		t.Fatalf("non-empty browser queries are not routed through full search before node-only browsing: %s", refresh)
	}
	artifactStart := strings.Index(application, "async function selectArtifactSearchResult(result,focusContent=true){")
	artifactEnd := strings.Index(application, "function renderList(){")
	if artifactStart < 0 || artifactEnd <= artifactStart {
		t.Fatal("browser artifact-detail loader is missing")
	}
	artifactLoader := application[artifactStart:artifactEnd]
	artifactAwait := strings.Index(artifactLoader, "detail=await fetchJSON(")
	artifactRevisionCheck := strings.Index(artifactLoader, "if(atlasRevision!==state.atlasRevision)throw snapshotGenerationError(")
	artifactMutation := strings.Index(artifactLoader, "state.artifacts.set(")
	if artifactAwait < 0 || artifactRevisionCheck <= artifactAwait || artifactMutation <= artifactRevisionCheck {
		t.Fatalf("artifact loader does not recheck the caller-captured atlas revision before mutation: %s", artifactLoader)
	}
}

func TestBrowserBodyOnlyArtifactSearchResultIsVisibleAndNavigable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; generated asset contract tests remain active")
	}
	bundle := exportFixture(t.TempDir(), "artifact-body-search.go", []byte("package artifactbodysearch\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	if !strings.HasSuffix(application, "boot();") {
		t.Fatal("browser application boot suffix is missing")
	}
	application = strings.TrimSuffix(application, "boot();")

	const prelude = `
const testElements=new Map();
function testElement(id){
  if(!testElements.has(id))testElements.set(id,{value:'',innerHTML:'',textContent:'',className:'',hidden:false,disabled:false,
    setAttribute(){},addEventListener(){},removeEventListener(){},focus(){},select(){},querySelectorAll(){return[]},
    classList:{toggle(){},add(){},remove(){}}});
  return testElements.get(id);
}
global.document={getElementById:testElement,querySelectorAll(){return[]},activeElement:null,body:{classList:{toggle(){}}}};
global.window={addEventListener(){}};
global.location={hash:'',pathname:'/',search:''};
global.history={replaceState(){}};
global.sessionStorage={getItem(){return null},setItem(){},removeItem(){}};
global.HTMLInputElement=class{};global.HTMLTextAreaElement=class{};global.HTMLSelectElement=class{};
`
	const harness = `
function repositoryResponse(data){return{ok:true,status:200,headers:{get(name){return name===snapshotGenerationHeader?'snapshot-body':null}},json:async()=>data}}
function atlas(){return{snapshot:{id:'snapshot-body',root_name:'body-search'},nodes:[],artifacts:[],edges:[],evidence:[],diagnostics:[]}}
(async()=>{
  const bodyOnlyTerm='bodyonlyartifactneedle',artifactID='artifact-body',nodeID='node-body';
  state.api=true;state.atlasRevision=1;state.searchRevision=1;state.bundle=atlas();state.coverage={};
  testElement('search').value=bodyOnlyTerm;testElement('kind').value='';testElement('language').value='';
  const requests=[];
  global.fetch=async path=>{
    requests.push(String(path));
    if(String(path).startsWith('/api/v1/search?'))return repositoryResponse({query:bodyOnlyTerm,hits:[{document:{id:artifactID,object_type:'artifact',kind:'file',language:'yaml',title:'service.yaml',qualified_name:'config/service.yaml',path:'config/service.yaml',body:'ordinary prefix '+bodyOnlyTerm+' grounded suffix',metadata:{rkc_secret_redacted:'true'}},score:9.5,reasons:['body:'+bodyOnlyTerm],terms:[bodyOnlyTerm]}],truncated:false,mode:'lexical',index_version:'test'});
    if(String(path)==='/api/v1/artifacts/'+artifactID)return repositoryResponse({artifact:{id:artifactID,path:'config/service.yaml',kind:'file',language:'yaml',media_type:'application/yaml',size_bytes:48,line_count:2,text:true,status:'syntax_parsed',sha256:'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'},nodes:[{id:nodeID,kind:'function',name:'LoadService',qualified_name:'config.LoadService',language:'go',artifact_id:artifactID}]});
    if(String(path)==='/api/v1/nodes/'+nodeID)return repositoryResponse({node:{id:nodeID,kind:'function',name:'LoadService',qualified_name:'config.LoadService',language:'go',artifact_id:artifactID},evidence:[],outgoing_edges:[],incoming_edges:[]});
    throw new Error('unexpected request '+path);
  };
  await refreshAPIList(state.searchRevision);
  if(!requests[0]?.startsWith('/api/v1/search?')||!requests[0].includes('q='+bodyOnlyTerm)||!requests[0].includes('object_types=node%2Cartifact'))throw new Error('body query did not use bounded full search: '+requests[0]);
  const result=state.apiSearchResults?.[0];
  if(!result||result.object_type!=='artifact'||Object.hasOwn(result,'body')||!result.excerpt.includes(bodyOnlyTerm)||state.results[0]?.objectType!=='artifact')throw new Error('body-only artifact hit was not normalized into bounded UI state');
  if(!testElement('list').innerHTML.includes('config/service.yaml')||!testElement('list').innerHTML.includes(bodyOnlyTerm)||!testElement('list').innerHTML.includes('artifact · file'))throw new Error('body-only artifact hit is not visible in the result list');
  await selectSearchResult('artifact',artifactID,false);
  if(state.selectedArtifact!==artifactID||state.selected!==null||!testElement('content').innerHTML.includes('Matched repository text')||!testElement('content').innerHTML.includes(bodyOnlyTerm)||!testElement('content').innerHTML.includes('LoadService'))throw new Error('artifact result did not navigate to grounded artifact detail');
  await selectSearchResult('node',nodeID,false);
  if(state.selected!==nodeID||state.selectedArtifact!==null||!testElement('content').innerHTML.includes('config.LoadService'))throw new Error('node navigation regressed after heterogeneous search');
  console.log('artifact-body-search-ok');
})().catch(error=>{console.error(error?.stack||error);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + application + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser body-only artifact search adversary failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("artifact-body-search-ok")) {
		t.Fatalf("browser body-only artifact search adversary did not finish: %s", output)
	}
}

func TestBrowserStaleRepositoryResponsesCannotRepopulateActivatedAtlas(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; generated asset contract tests remain active")
	}
	bundle := exportFixture(t.TempDir(), "stale-read.go", []byte("package staleread\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	if !strings.HasSuffix(application, "boot();") {
		t.Fatal("browser application boot suffix is missing")
	}
	application = strings.TrimSuffix(application, "boot();")

	const prelude = `
const testElements=new Map();
function testElement(id){
  if(!testElements.has(id))testElements.set(id,{value:'',innerHTML:'',textContent:'',className:'',hidden:false,disabled:false,
    setAttribute(){},addEventListener(){},removeEventListener(){},focus(){},select(){},querySelectorAll(){return[]},
    classList:{toggle(){},add(){},remove(){}}});
  return testElements.get(id);
}
global.document={getElementById:testElement,querySelectorAll(){return[]},activeElement:null,body:{classList:{toggle(){}}}};
global.window={addEventListener(){}};
global.location={hash:'',pathname:'/',search:''};
global.history={replaceState(){}};
global.sessionStorage={getItem(){return null},setItem(){},removeItem(){}};
global.HTMLInputElement=class{};global.HTMLTextAreaElement=class{};global.HTMLSelectElement=class{};
`
	const harness = `
function deferredResponse(){let resolve;return{promise:new Promise(done=>{resolve=done}),resolve}}
function repositoryResponse(snapshotID,data,afterHeader){let scheduled=false;return{ok:true,status:200,headers:{get(name){if(name!==snapshotGenerationHeader)return null;if(afterHeader&&!scheduled){scheduled=true;queueMicrotask(afterHeader)}return snapshotID}},json:async()=>data}}
function atlas(snapshotID,nodes=[]){return{snapshot:{id:snapshotID,root_name:snapshotID},nodes,artifacts:[],edges:[],evidence:[],diagnostics:[]}}
(async()=>{
  state.api=true;state.atlasRevision=1;state.searchRevision=1;state.bundle=atlas('old');state.coverage={};
  state.nodes.clear();state.evidence.clear();state.outgoing.clear();state.incoming.clear();

  const detail=deferredResponse();global.fetch=()=>detail.promise;
  const pendingDetail=loadAPINode('stale-detail');
  advanceAtlasGeneration();state.bundle=atlas('new');state.nodes.clear();
  detail.resolve(repositoryResponse('old',{node:{id:'stale-detail',name:'Stale detail'},evidence:[],outgoing_edges:[],incoming_edges:[]}));
  let detailRejected=false;try{await pendingDetail}catch(error){detailRejected=isSnapshotGenerationError(error)}
  if(!detailRejected||state.nodes.has('stale-detail'))throw new Error('stale detail response repopulated the activated atlas');

  state.bundle=atlas('detail-validated');state.nodes.clear();
  const postValidationDetail=deferredResponse();global.fetch=()=>postValidationDetail.promise;
  const pendingPostValidationDetail=loadAPINode('post-validation-detail');
  postValidationDetail.resolve(repositoryResponse('detail-validated',{node:{id:'post-validation-detail',name:'Post-validation detail'},evidence:[],outgoing_edges:[],incoming_edges:[]},()=>{advanceAtlasGeneration();state.bundle=atlas('detail-activated');state.nodes.clear()}));
  let postValidationDetailRejected=false;try{await pendingPostValidationDetail}catch(error){postValidationDetailRejected=isSnapshotGenerationError(error)}
  if(!postValidationDetailRejected||state.bundle.snapshot.id!=='detail-activated'||state.nodes.has('post-validation-detail'))throw new Error('detail response mutated state after fetch validation but before caller continuation');

  const seed={id:'seed',name:'Current seed',kind:'function'};state.bundle=atlas('new',[seed]);state.nodes.set(seed.id,seed);
  const graph=deferredResponse();global.fetch=()=>graph.promise;testElement('content').innerHTML='old graph';
  const pendingGraph=renderGraph(seed.id);
  advanceAtlasGeneration();state.bundle=atlas('newer');state.nodes.clear();testElement('content').innerHTML='new atlas visible';
  graph.resolve(repositoryResponse('new',{nodes:[{id:'stale-neighbor',name:'Stale neighbor',kind:'function'}],edges:[{id:'stale-edge',from:'seed',to:'stale-neighbor'}]}));
  await pendingGraph;
  if(state.nodes.has('stale-neighbor')||testElement('content').innerHTML!=='new atlas visible')throw new Error('stale graph response replaced the activated atlas view');

  const postSeed={id:'post-seed',name:'Post-validation seed',kind:'function'};state.bundle=atlas('graph-validated',[postSeed]);state.nodes.set(postSeed.id,postSeed);
  const postValidationGraph=deferredResponse();global.fetch=()=>postValidationGraph.promise;testElement('content').innerHTML='graph loading';
  const pendingPostValidationGraph=renderGraph(postSeed.id);
  postValidationGraph.resolve(repositoryResponse('graph-validated',{nodes:[{id:'post-validation-neighbor',name:'Post-validation neighbor',kind:'function'}],edges:[{id:'post-validation-edge',from:'post-seed',to:'post-validation-neighbor'}]},()=>{advanceAtlasGeneration();state.bundle=atlas('graph-activated');state.nodes.clear();testElement('content').innerHTML='post-validation atlas visible'}));
  await pendingPostValidationGraph;
  if(state.bundle.snapshot.id!=='graph-activated'||state.nodes.has('post-validation-neighbor')||testElement('content').innerHTML!=='post-validation atlas visible')throw new Error('graph response mutated state after fetch validation but before caller continuation');

  testElement('search').value='';testElement('kind').value='';testElement('language').value='';
  const requestedSearchRevision=state.searchRevision,search=deferredResponse();global.fetch=()=>search.promise;
  const pendingSearch=refreshAPIList(requestedSearchRevision);
  advanceAtlasGeneration();state.bundle=atlas('latest');state.nodes.clear();testElement('result-summary').textContent='latest atlas ready';
  search.resolve(repositoryResponse('graph-activated',{items:[{id:'stale-search',name:'Stale search'}],truncated:false}));
  await pendingSearch;
  if(state.bundle.snapshot.id!=='latest'||state.nodes.has('stale-search')||testElement('result-summary').textContent!=='latest atlas ready')throw new Error('stale search response repopulated the activated atlas');

  const wrongSnapshot=deferredResponse();global.fetch=()=>wrongSnapshot.promise;
  const pendingWrongSnapshot=loadAPINode('wrong-snapshot');
  wrongSnapshot.resolve(repositoryResponse('different',{node:{id:'wrong-snapshot'},evidence:[],outgoing_edges:[],incoming_edges:[]}));
  let snapshotRejected=false;try{await pendingWrongSnapshot}catch(error){snapshotRejected=isSnapshotGenerationError(error)}
  if(!snapshotRejected||state.nodes.has('wrong-snapshot'))throw new Error('wrong-snapshot detail response was accepted');

  state.bundle=atlas('artifact-validated');state.artifacts.clear();state.selectedArtifact=null;
  const artifactResult={id:'stale-artifact',object_type:'artifact',kind:'file',title:'stale.txt',path:'stale.txt',terms:[],reasons:[],excerpt:'stale'};
  const postValidationArtifact=deferredResponse();global.fetch=()=>postValidationArtifact.promise;
  const pendingPostValidationArtifact=selectArtifactSearchResult(artifactResult,false);
  postValidationArtifact.resolve(repositoryResponse('artifact-validated',{artifact:{id:'stale-artifact',path:'stale.txt',kind:'file',text:true,status:'parsed'},nodes:[]},()=>{advanceAtlasGeneration();state.bundle=atlas('artifact-activated');state.artifacts.clear();state.selectedArtifact=null}));
  let postValidationArtifactRejected=false;try{await pendingPostValidationArtifact}catch(error){postValidationArtifactRejected=isSnapshotGenerationError(error)}
  if(!postValidationArtifactRejected||state.bundle.snapshot.id!=='artifact-activated'||state.artifacts.has('stale-artifact')||state.selectedArtifact)throw new Error('artifact detail mutated state after fetch validation but before caller continuation');
  console.log('stale-read-guard-ok');
})().catch(error=>{console.error(error?.stack||error);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + application + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser stale-response adversary failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("stale-read-guard-ok")) {
		t.Fatalf("browser stale-response adversary did not finish: %s", output)
	}
}

func TestBrowserResourceAndCancellationClaimsAreStateBound(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "resource-claims.go", []byte("package resourceclaims\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"enabled?'1 CPU · 4.5 GiB hard ceiling · re-proved continuously':'No command execution'",
		"enabled?number(session.maximum_output_bytes)+' bytes':'Not applicable'",
		"canceled:'Canceled'",
		"cleanup_failed:'Cleanup unproven'",
		"helper-launching model, Python, remote acquisition, and live history operations",
		"trusted browser profile",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser resource-truth contract is missing %q", marker)
		}
	}
	if strings.Contains(application, "Canceled safely") || strings.Contains(application, "Canceling safely") {
		t.Fatal("browser overclaims cancellation safety")
	}
}

func TestBrowserAssetsProvideAuthenticatedGuidedFolderSelection(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "folders.go", []byte("package folders\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"Guided first run", "Analyze a folder", "repository-folder",
		"browseWorkbenchDirectory", "renderWorkbenchDirectory",
		"selectRepositoryFolder", "analyzeRepositoryFolder",
		"/api/v1/workbench/directories?", "X-RKC-Workbench-Token",
		"selectedRepositoryDefaults", "quickstart:[folder]",
		"flow:['report','--dir',atlas]", "trace:['report','--dir',atlas]",
		"a model is not required", "Folder response is invalid",
		"default_executable", "Workbench boundary:", "CLI only",
		"activated_dataset", "loadActivatedWorkbenchDataset",
		"Activated snapshot identity does not match", "Workbench defaults are not bound",
		"Atlas activated:", "Overview, search, graph, and command defaults",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser guided folder flow is missing %q", marker)
		}
	}
	if strings.Contains(application, "webkitdirectory") {
		t.Fatal("browser uses an upload picker that cannot select the host folder path")
	}
	styles := string(assets["styles.css"])
	for _, marker := range []string{".folder-controls", ".folder-browser[hidden]", ".folder-list", ".folder-choice"} {
		if !strings.Contains(styles, marker) {
			t.Errorf("browser folder flow is missing responsive style %q", marker)
		}
	}
}
