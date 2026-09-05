package export

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

// Run the shipped browser code, not a second implementation of the retrieval
// workflow. The adversary crosses an activation boundary while a packet is in flight.
func TestBrowserContextPacketsRemainSnapshotBoundAndQuoteUntrustedText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	bundle := exportFixture(t.TempDir(), "context.go", []byte("package context\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	app := strings.TrimSuffix(string(assets["app.js"]), "boot();")
	const prelude = `
const elements=new Map();
function element(id){if(!elements.has(id))elements.set(id,{value:'',textContent:'',innerHTML:'',hidden:false,disabled:false,focus(){},setAttribute(){},addEventListener(){},classList:{toggle(){}},querySelectorAll(){return[]}});return elements.get(id)}
global.document={getElementById:element,querySelectorAll(){return[]},body:{classList:{toggle(){}}}};
global.window={addEventListener(){}};global.location={hash:'',pathname:'/',search:''};
`
	const harness = `
(async()=>{
 state.api=true;state.atlasRevision=1;state.view='outputs';state.bundle={snapshot:{id:'snapshot-a'}};
 element('context-query').value='Login';element('context-limit').value='12';element('context-budget').value='32768';
 const packet={schema_version:'rkc-context/v1',snapshot_id:'snapshot-a',query:'Login',items:[{citation_id:'citation-1',object_id:'object-1',object_type:'node',title:'<script>alert(1)</script>',path:'src/auth.go',text:'[run](javascript:alert(1))\n<script>bad()</script>'}],bytes:320,truncated:false,warnings:['<img src=x onerror=alert(1)>'],digest:'sha256-test'};
 let requestPath='';global.fetch=async path=>{requestPath=path;return{ok:true,headers:{get(){return 'snapshot-a'}},json:async()=>packet}};
 await buildContextPacket();
 if(!requestPath.startsWith('/api/v1/context?')||!requestPath.includes('max_bytes=32768')||state.contextPacket!==packet)throw Error('bounded context did not reach ready state');
 const preview=element('context-result').innerHTML;
 if(preview.includes('<script>')||preview.includes('<img src=x')||!preview.includes('&lt;script&gt;'))throw Error('repository context was interpreted as HTML');
 const markdown=contextMarkdown(packet);
 if(!markdown.includes('Snapshot: snapshot-a')||!markdown.includes('citation-1')||markdown.includes('> [run](javascript:'))throw Error('Markdown lost attribution or exposed active repository Markdown');
 let finish;global.fetch=()=>new Promise(resolve=>{finish=resolve});
 const pending=buildContextPacket();advanceAtlasGeneration();state.bundle={snapshot:{id:'snapshot-b'}};state.contextPacket=null;
 finish({ok:true,headers:{get(){return 'snapshot-a'}},json:async()=>packet});await pending;
 if(state.contextPacket)throw Error('stale packet crossed snapshot activation');
 if(contextPreview({snapshot_id:'snapshot-b',query:'missing',items:[],warnings:[],bytes:2}).includes('No matching evidence found.')===false)throw Error('empty retrieval was presented as useful evidence');
 element('pack-sources').value='/tmp/atlas with spaces\n/tmp/$(touch nope)';element('pack-output').value='/tmp/pack with spaces';
 applyWorkflowSettings();const args=parseCommandArguments(element('command-args').value);
 if(JSON.stringify(args)!==JSON.stringify(['build','--out','/tmp/pack with spaces','/tmp/atlas with spaces','/tmp/$(touch nope)']))throw Error('guided knowledge paths lost exact argument boundaries');
 clearTimeout(state.toastTimer);
 console.log('context-ux-ok');
})().catch(error=>{console.error(error);process.exitCode=1});
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(prelude + app + harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser context adversary: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "context-ux-ok") {
		t.Fatalf("browser context test incomplete: %s", output)
	}
}
