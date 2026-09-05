package export

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserGitHubSourcesKeepCredentialsEphemeralAndResultsCurrent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	bundle := exportFixture(t.TempDir(), "github.go", []byte("package github\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := strings.TrimSuffix(string(assets["app.js"]), "boot();")
	const prelude = `
const elements=new Map();
function element(id){
 if(!elements.has(id))elements.set(id,{value:'',innerHTML:'',textContent:'',hidden:false,disabled:false,handlers:{},
 setAttribute(key,value){this[key]=value},getAttribute(key){return this[key]||''},addEventListener(key,fn){this.handlers[key]=fn},focus(){},select(){},scrollIntoView(){},before(){},append(){},querySelectorAll(){return[]},querySelector(){return element('nested')},classList:{toggle(){},add(){},remove(){}}});
 return elements.get(id);
}
global.document={getElementById:element,querySelectorAll(){return[]},addEventListener(){},body:{classList:{toggle(){}}}};
global.window={addEventListener(){}};global.location={pathname:'/',search:'',hash:''};global.history={replaceState(){}};
global.sessionStorage={getItem(){return null},setItem(){throw Error('GitHub credentials must not enter browser storage')},removeItem(){}};
`
	const harness = `
function assert(value,message){if(!value)throw new Error(message)}
function response(data,status=200){return{ok:status>=200&&status<300,status,json:async()=>data}}
function deferred(){let resolve;return{promise:new Promise(done=>resolve=done),resolve}}
function repository(name='project'){return{full_name:'public-owner/'+name,description:'Public <script>fixture</script>',html_url:'https://github.com/public-owner/'+name,default_branch:'main',private:false}}
function results(items,page=1,total=items.length){return{items,total,incomplete:false,next_page:page*25<total?page+1:0}}
function reset(){
 clearTimeout(state.toastTimer);elements.clear();
 Object.assign(state,{api:true,view:'sources',sourceProvider:'github',atlasRevision:1,navigationRevision:0,activeJob:null,submittingJob:false,pendingActivation:null,activationLoading:false,lastJob:null,jobError:'',jobPolling:false,jobCanceling:false,workbench:{enabled:true,token:'workbench-session',commands:defaultCommands(),folder_compilation_only:true}});
 state.bundle={snapshot:{id:'snapshot',metadata:{rkc_workspace:'empty'}}};state.coverage={};
 state.github={session:{connected:false},sessionLoading:false,sessionRevision:0,authPending:false,authError:'',draft:'',query:'',page:1,items:[],total:0,incomplete:false,nextPage:null,requestRevision:0,loading:false,error:'',selected:null};
 renderSourceChooser();
}
function query(value){state.github.draft=value;resetGitHubResults();renderGitHubResults()}
(async()=>{
 reset();let reads=0;global.fetch=async()=>{reads++;return response(results([]))};await searchGitHubRepositories();assert(reads===0,'empty GitHub query was sent');
 assert(element('github-account').innerHTML.includes('type="password"')&&element('github-account').innerHTML.includes('not saved in browser storage'),'private connection lacks password input or session-only explanation');
 query('project');global.fetch=async(path,options)=>{reads++;assert(path==='/api/v1/workbench/github/repositories?q=project&page=1'&&options.headers['X-RKC-Workbench-Token']==='workbench-session','search was not authenticated or used wrong query transport');return response(results([repository()]))};
 await searchGitHubRepositories();assert(reads===1&&state.github.items.length===1&&!state.github.session.connected,'public search requires a GitHub connection');
 assert(element('github-results').innerHTML.includes('&lt;script&gt;')&&!element('github-results').innerHTML.includes('<script>fixture'),'GitHub description was not escaped');

 for(const fails of [false,true]){
  reset();const old=deferred();query('old');global.fetch=()=>old.promise;const older=searchGitHubRepositories();query('current');global.fetch=async()=>response(results([repository('current')]));await searchGitHubRepositories();const html=element('github-results').innerHTML;
  old.resolve(response(fails?{detail:'obsolete search error'}:results([repository('old')]),fails?500:200));await older;
  assert(state.github.query==='current'&&state.github.items[0].full_name.endsWith('/current')&&!state.github.error&&element('github-results').innerHTML===html,'stale GitHub search replaced newer results or raised an error');
 }
 reset();query('many');let requests=0;const secondPage=deferred();global.fetch=async path=>{requests++;const page=Number(new URL(path,'http://local').searchParams.get('page'));return page===2?secondPage.promise:response(results(Array.from({length:25},(_,i)=>repository('item-'+i)),1,61))};
 await searchGitHubRepositories();const changing=searchGitHubRepositories(2);await searchGitHubRepositories(2);assert(requests===2,'duplicate page click submitted concurrent searches');
 secondPage.resolve(response(results(Array.from({length:25},(_,i)=>repository('item-'+(25+i))),2,61)));await changing;assert(state.github.items.length===25&&state.github.page===2&&state.github.nextPage===3,'next page appended unbounded records or lost navigation');
 global.fetch=async()=>response({detail:'retryable page failure'},502);await searchGitHubRepositories(3);assert(state.github.page===2&&state.github.items.length===25&&state.github.error.includes('retryable'),'failed page destroyed the current page');
 global.fetch=async()=>response(results(Array.from({length:11},(_,i)=>repository('item-'+(50+i))),3,61));await searchGitHubRepositories(3);assert(state.github.page===3&&state.github.items.length===11&&!state.github.nextPage,'terminal zero next_page was not accepted');
 global.fetch=async()=>response(results(Array.from({length:25},(_,i)=>repository('item-'+i)),1,61));await searchGitHubRepositories(1);assert(state.github.page===1&&state.github.items[0].full_name.endsWith('/item-0'),'previous page was not refetched');

 for(const data of [results(Array.from({length:26},(_,i)=>repository('oversized-'+i))),results([{...repository(),html_url:'javascript:alert(1)'}]),results([repository(),repository()]),{...results([repository()]),next_page:8}]){
  reset();query('invalid');global.fetch=async()=>response(data);await searchGitHubRepositories();assert(state.github.error.includes('invalid result page')&&state.github.items.length===0,'malformed GitHub page entered the UI');
 }
 reset();query('old-atlas');const stale=deferred();global.fetch=()=>stale.promise;const searching=searchGitHubRepositories();advanceAtlasGeneration();stale.resolve(response(results([repository()])));await searching;assert(state.github.items.length===0&&!state.github.loading,'old-atlas GitHub result survived activation');
 reset();query('away');const away=deferred();global.fetch=()=>away.promise;const background=searchGitHubRepositories();state.view='coverage';element('content').innerHTML='new view';away.resolve(response(results([repository()])));await background;assert(element('content').innerHTML==='new view','background GitHub completion stole content navigation');

 reset();state.github.session=null;const staleSession=deferred();global.fetch=()=>staleSession.promise;const probing=loadGitHubSession();element('github-token').value='test-only';let credentialBodies=0;
 global.fetch=async(path,options)=>{credentialBodies++;assert(path==='/api/v1/workbench/github/session'&&!path.includes('?')&&options.method==='POST'&&JSON.parse(options.body).token==='test-only'&&!element('github-token').value,'token was not confined to the body and cleared from the field');return response({connected:true,login:'test-user'})};
 await changeGitHubConnection('POST');staleSession.resolve(response({connected:false}));await probing;
 assert(credentialBodies===1&&state.github.session.connected&&state.github.session.login==='test-user'&&!JSON.stringify(state.github).includes('test-only'),'stale session discovery replaced a new connection or credential entered application state');
 state.github.items=[{...repository(),private:true}];state.github.selected=state.github.items[0];const disconnect=deferred();let disconnects=0;global.fetch=()=>{disconnects++;return disconnect.promise};const disconnecting=changeGitHubConnection('DELETE');await changeGitHubConnection('DELETE');assert(disconnects===1&&!state.github.items.length&&!state.github.selected,'disconnect was duplicated or retained private results');disconnect.resolve(response({connected:false}));await disconnecting;assert(!state.github.session.connected&&!state.github.authPending,'disconnect did not reset connection state');

 reset();element('github-token').value='test-only';global.fetch=async(_path,options)=>options.method==='POST'?response({detail:'server echoed test-only'},403):response({connected:false});await changeGitHubConnection('POST');assert(state.github.authError.includes('could not connect')&&!element('github-account').innerHTML.includes('test-only')&&!state.github.authPending,'failed connection exposed token detail or could not be retried');
 reset();query('before-connect');const oldSearch=deferred();global.fetch=()=>oldSearch.promise;const preConnection=searchGitHubRepositories();element('github-token').value='test-only';global.fetch=async()=>response({connected:true,login:'test-user'});await changeGitHubConnection('POST');oldSearch.resolve(response(results([repository()])));await preConnection;assert(!state.github.items.length,'a search using the old GitHub connection survived replacement');

 reset();assert(canSubmitWorkbenchJob({github_repository:'public-owner/project'})&&!canSubmitWorkbenchJob({github_repository:'../outside'})&&!canSubmitWorkbenchJob({github_repository:'public-owner/project',args:[]}), 'GitHub source payload validation accepts invalid or mixed requests');
 let posted;global.fetch=async(path,options)=>{if(options.method==='POST'){posted=JSON.parse(options.body);return response({id:'github-job',status:'queued'},202)}return response({id:'github-job',status:'failed',error:'fixture acquisition failure'})};
 await submitWorkbenchJob({github_repository:'public-owner/project'});assert(posted.github_repository==='public-owner/project'&&!posted.args&&state.lastJob.status==='failed'&&!state.activeJob,'GitHub compilation did not use the shared job lifecycle');
 assert(jobOutputText({github_source:{repository:'public-owner/project',commit_sha:'pinned-fixture',archive_sha256:'digest-fixture'}}).includes('Pinned commit: pinned-fixture'),'pinned GitHub provenance is absent from scan details');
 clearTimeout(state.toastTimer);console.log('github-source-lifecycle-ok');
})().catch(error=>{clearTimeout(state.toastTimer);console.error(error.stack);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + application + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("GitHub source lifecycle failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "github-source-lifecycle-ok") {
		t.Fatalf("missing GitHub lifecycle completion: %s", output)
	}
}
