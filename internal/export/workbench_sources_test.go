package export

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserSourceWelcomeAndJobLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	bundle := exportFixture(t.TempDir(), "welcome.go", []byte("package welcome\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := strings.TrimSuffix(string(assets["app.js"]), "boot();")
	const prelude = `
const elements=new Map(),classes=new Map();
function element(id){
 if(!elements.has(id))elements.set(id,{value:'',innerHTML:'',textContent:'',hidden:false,disabled:false,handlers:{},
 setAttribute(key,value){this[key]=value},getAttribute(key){return this[key]||''},addEventListener(key,fn){this.handlers[key]=fn},focus(){},select(){},scrollIntoView(){},before(){},append(){},
 querySelectorAll(){return[]},querySelector(){return element('nested')},classList:{toggle(){},add(){},remove(){}}});
 return elements.get(id);
}
global.document={getElementById:element,querySelectorAll(){return[]},addEventListener(){},createElement(){return element('created')},createTextNode(text){return text},body:{classList:{toggle(key,value){classes.set(key,value)}}}};
global.window={addEventListener(){}};let fragment='',storedToken='';
global.location={pathname:'/',search:'',get hash(){return fragment},set hash(value){fragment=value&&!value.startsWith('#')?'#'+value:value}};
global.history={replaceState(_state,_title,url){fragment=url.includes('#')?url.slice(url.indexOf('#')):''}};
global.sessionStorage={getItem(){return storedToken},setItem(_key,value){storedToken=value},removeItem(){storedToken=''}};
`
	const harness = `
function assert(value,message){if(!value)throw new Error(message)}
function deferred(){let resolve;return{promise:new Promise(done=>resolve=done),resolve}}
const settle=()=>new Promise(resolve=>setImmediate(resolve));
function response(data,status=200,snapshot='empty-snapshot'){return{ok:status===200,status,headers:{get(){return snapshot}},json:async()=>data}}
function session(portable=false){return{enabled:true,token:'test-session',workspace:'/home/test',folder_compilation_only:portable,commands:defaultCommands(),maximum_output_bytes:10000}}
function reset(){
 clearTimeout(state.toastTimer);elements.clear();classes.clear();fragment='';storedToken='';
 Object.assign(state,{api:true,atlasRevision:1,navigationRevision:0,directoryRevision:0,view:'sources',workbench:session(),sourceFolder:'',repositoryFolder:'',directoryListing:null,jobError:'',jobPolling:false,jobCanceling:false,pendingActivation:null,activationLoading:false,activeJob:null,lastJob:null,submittingJob:false,selected:null,selectedArtifact:null,activationNotice:null,staticBootstrap:false,staticSearchRecords:null,staticSearchByID:new Map(),commandName:'quickstart'});
 state.bundle={snapshot:{id:'empty-snapshot',root_name:'Your workspace',metadata:{rkc_workspace:'empty'}},nodes:[],artifacts:[],edges:[],evidence:[],diagnostics:[]};state.coverage={nodes_total:0};state.facets={};state.nodes.clear();state.artifacts.clear();state.evidence.clear();state.outgoing.clear();state.incoming.clear();state.commandDrafts.clear();
}
function initialFetch(path){
 const snapshot=state.bundle.snapshot.id;
 if(path==='/api/v1/health')return response({snapshot_id:snapshot},200,snapshot);
 if(path==='/api/v1/manifest')return response(state.bundle.snapshot,200,snapshot);
 if(path==='/api/v1/coverage')return response({snapshot_id:snapshot,nodes_total:0},200,snapshot);
 if(path==='/api/v1/facets')return response({},200,snapshot);
 if(path.startsWith('/api/v1/nodes?')||path.startsWith('/api/v1/diagnostics?'))return response({snapshot_id:snapshot,total:0,items:[]},200,snapshot);
 throw new Error('Unexpected request '+path);
}
(async()=>{
 reset();state.workbench=null;location.hash='rkc-workbench=test-bootstrap';let exchanges=0;
 global.fetch=async(path,options)=>{if(path==='/api/v1/workbench/session'){exchanges++;assert(options.headers['X-RKC-Workbench-Bootstrap']==='test-bootstrap'&&!location.hash,'bootstrap was not removed before authenticated exchange');return response(session(true))}return initialFetch(path)};
 await boot();assert(exchanges===1&&storedToken==='test-session'&&state.view==='sources'&&isEmptyWorkspace(),'empty startup did not exchange authentication and open source choice');
 assert(element('content').innerHTML.includes('Make sense of your source.')&&classes.get('empty-workspace')&&classes.get('source-view'),'empty startup exposed atlas chrome');
 assert(element('runtime-status').textContent==='Local folder workspace'&&!element('content').innerHTML.includes('OAuth'),'portable session claims containment or promises OAuth');
 assert(!element('snapshot').textContent.startsWith('Snapshot'),'empty workspace claims an analyzed snapshot');

 reset();state.workbench=null;global.fetch=async(path)=>path==='/api/v1/workbench/session'?response({},403):initialFetch(path);
 await boot();assert(!state.workbench.enabled&&element('content').innerHTML.includes('Connect your local session')&&!storedToken,'unauthenticated empty workspace exposed execution');
 let writes=0;global.fetch=async()=>{writes++;return response({})};await submitWorkbenchJob({args:['quickstart','/tmp/source']});assert(writes===0,'unauthenticated submission reached the write API');

 reset();storedToken='already-authorized';global.fetch=async()=>{throw new Error('temporary disconnection')};await probeWorkbench();
 assert(!state.workbench.enabled&&storedToken==='already-authorized','transport failure discarded the valid reconnect credential');
 global.fetch=async(_path,options)=>{assert(options.headers['X-RKC-Workbench-Token']==='already-authorized','reconnect did not retry the stored session');return response(session())};await probeWorkbench();assert(state.workbench.enabled,'temporary session failure could not reconnect');
 global.fetch=async()=>response({},403);await probeWorkbench();assert(!storedToken&&!state.workbench.enabled,'rejected authentication retained an invalid credential');

 reset();state.workbench=null;const slowSession=deferred(),ready=deferred();
 global.fetch=async path=>{if(path==='/api/v1/workbench/session'){ready.resolve();return slowSession.promise}return initialFetch(path)};
 const booting=boot();await ready.promise;setView('sources',false);state.sourceFolder='/typed/during-startup';slowSession.resolve(response(session()));await booting;
 assert(state.view==='sources'&&element('content').innerHTML.includes('/typed/during-startup')&&element('content').innerHTML.includes('id="analyze-folder"'),'startup authentication lost source intent or left a disabled chooser');

 reset();renderSourceChooser();let directoryHeaders;
 global.fetch=async(path,options)=>{directoryHeaders=options.headers;return response({path:'/sources',parent:'/',directories:[{path:'/sources/project',name:'Project'}]})};
 await browseWorkbenchDirectory('');assert(directoryHeaders['X-RKC-Workbench-Token']==='test-session'&&state.directoryListing.path==='/sources','folder browser was not authenticated');
 selectRepositoryFolder('/sources/project');assert(state.sourceFolder==='/sources/project'&&element('repository-folder').value==='/sources/project'&&element('folder-browser').hidden&&!element('analyze-folder').disabled,'selected folder did not populate the start action');
 const oldListing=deferred();global.fetch=()=>oldListing.promise;const pendingBrowse=browseWorkbenchDirectory('/old');selectRepositoryFolder('/new');oldListing.resolve(response({path:'/old',directories:[]}));await pendingBrowse;
 assert(state.sourceFolder==='/new'&&state.directoryListing===null,'late folder listing replaced the chosen folder');
 for(const fails of [false,true]){
  const old=deferred();global.fetch=()=>old.promise;const browsing=browseWorkbenchDirectory('/old');state.bundle.snapshot.metadata={};setView('coverage',false);const visible=element('content').innerHTML;old.resolve(response(fails?{detail:'stale directory failure'}:{path:'/old',directories:[]},fails?500:200));await browsing;
  assert(element('content').innerHTML===visible,'late directory response disturbed a newer view');
 }

 reset();state.workbench=session(true);
 assert(canRunWorkbenchArguments(['quickstart','C:\\Projects\\My repo'])&&!canRunWorkbenchArguments(['quickstart','--help'])&&!canRunWorkbenchArguments(['quickstart','/tmp','--semantic'])&&!canRunWorkbenchArguments(['scan','/tmp']),'portable allowed arguments exceed one-folder compilation');
 state.view='commands';element('command-args').value='--help';renderCommands();assert(element('run-command').hidden&&element('command-status').textContent.includes('Copy'),'portable unsupported preset has a broken Run action');
 assert(element('content').innerHTML.includes('Built-in folder compilation · no external commands')&&!element('content').innerHTML.includes('1 CPU · 4.5 GiB hard ceiling'),'portable resource claims are inaccurate');
 element('command-args').value="'/tmp/My project'";updateWorkbenchJobUI();assert(!element('run-command').hidden&&!element('run-command').disabled,'valid portable quickstart cannot run');

 reset();renderSourceChooser();let submits=0;
 global.fetch=async(path,options)=>{if(options?.method==='POST'){submits++;assert(JSON.parse(options.body).args.join('|')==='quickstart|/tmp/My project','folder submission did not preserve the exact argument');return response({detail:'folder cannot be read'},400)}throw new Error('unexpected job read')};
 await analyzeRepositoryFolder('/tmp/My project');assert(state.jobError==='folder cannot be read'&&!state.activeJob&&!state.submittingJob&&element('analyze-folder').textContent==='Try compiling again','failed submission is not retryable');
 global.fetch=async(path,options)=>options?.method==='POST'?(submits++,response({id:'j1',status:'queued'})):response({id:'j1',status:'failed',error:'scan failed'});
 await analyzeRepositoryFolder('/tmp/My project');assert(submits===2&&state.lastJob.status==='failed'&&!state.activeJob&&element('source-job-error').textContent==='scan failed','failed scan is not visible and retryable');

 reset();renderSourceChooser();let posts=0;
 global.fetch=async(path,options)=>options?.method==='POST'?(posts++,response({id:'lost',status:'queued'})):response({detail:'connection interrupted'},503);
 await analyzeRepositoryFolder('/tmp/project');assert(state.activeJob==='lost'&&!state.jobPolling&&!element('source-resume').hidden&&element('analyze-folder').disabled,'lost status connection released the running job or concealed reconnect');
 await analyzeRepositoryFolder('/tmp/project');assert(posts===1,'retry submitted a second job while prior status was unknown');
 global.fetch=async()=>response({id:'lost',status:'succeeded'});await resumeWorkbenchJob();assert(!state.activeJob&&state.lastJob.status==='succeeded','status retry failed to release a verified finished job');

 reset();renderSourceChooser();const waiting=deferred(),pollStarted=deferred();let cancels=0;
 global.fetch=async(path,options)=>{if(options?.method==='POST')return response({id:'cancel-me',status:'queued'});if(options?.method==='DELETE'){cancels++;return response({id:'cancel-me',status:'running'})}pollStarted.resolve();return waiting.promise};
 const running=analyzeRepositoryFolder('/tmp/project');await pollStarted.promise;await cancelWorkbenchJob('cancel-me');await cancelWorkbenchJob('cancel-me');
 assert(cancels===1&&state.activeJob==='cancel-me'&&state.jobCanceling,'cancel either duplicated or claimed completion before proof');
 waiting.resolve(response({id:'cancel-me',status:'canceled'}));await running;assert(!state.activeJob&&!state.jobCanceling&&state.lastJob.status==='canceled'&&element('source-job-help').textContent.includes('Scan canceled'),'cancel completion was not presented');

 reset();renderSourceChooser();const identity={snapshot_id:'compiled',repository_id:'repo',repository_root:'/tmp/project',atlas_root:'/tmp/project/.rkc',root_name:'Project'};let compilePosts=0,invalidActivation=true;
 global.fetch=async(path,options)=>{
  if(options?.method==='POST'){compilePosts++;return response({id:'compiled-job',status:'queued'})}
  if(path==='/api/v1/workbench/jobs/compiled-job')return response({id:'compiled-job',status:'succeeded',activated_dataset:identity});
  if(path==='/api/v1/workbench/session')return response({...session(),active_dataset:identity},200,'compiled');
  const id=invalidActivation?'wrong-generation':'compiled';
  if(path==='/api/v1/health')return response({snapshot_id:id},200,id);
  if(path==='/api/v1/manifest')return response({id,root_name:'Project',repository_id:'repo'},200,id);
  if(path==='/api/v1/coverage')return response({snapshot_id:id,nodes_total:0},200,id);
  if(path==='/api/v1/facets')return response({},200,id);
  if(path.startsWith('/api/v1/nodes?')||path.startsWith('/api/v1/diagnostics?'))return response({snapshot_id:id,total:0,items:[]},200,id);
  throw new Error('unexpected activation path '+path);
 };
 await analyzeRepositoryFolder('/tmp/project');assert(state.pendingActivation&&state.view==='sources'&&state.jobError.includes('identity does not match')&&!element('source-resume').hidden,'activation mismatch did not offer safe reopen');
 invalidActivation=false;await resumeWorkbenchJob();
 assert(compilePosts===1&&!state.pendingActivation&&state.bundle.snapshot.id==='compiled'&&state.view==='overview'&&!classes.get('empty-workspace')&&!classes.get('source-view')&&!element('change-source').hidden,'activation retry rescanned or failed to restore the atlas workspace');
 setView('sources',false);assert(element('content').innerHTML.includes('Back to atlas'),'existing atlas cannot return from source choice');
 clearTimeout(state.toastTimer);console.log('source-welcome-job-lifecycle-ok');
})().catch(error=>{clearTimeout(state.toastTimer);console.error(error.stack);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + application + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("source welcome lifecycle failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "source-welcome-job-lifecycle-ok") {
		t.Fatalf("missing source lifecycle completion: %s", output)
	}
}
