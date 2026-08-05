package trajectoryviz

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
)

var pageTemplate = template.Must(template.New("trajectory").Parse(`<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{color-scheme:dark;--bg:#081018;--panel:#111c28;--line:#284057;--text:#e7f1f8;--muted:#8ea4b7;--accent:#5eead4;--job:#fb7185;--goal:#fbbf24}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.4 ui-sans-serif,system-ui,sans-serif}
header{display:flex;gap:18px;align-items:end;justify-content:space-between;padding:16px 20px;border-bottom:1px solid var(--line);background:#0b1621}
h1{font-size:18px;margin:0}.meta{color:var(--muted);font-size:12px}.metrics{display:flex;gap:18px;flex-wrap:wrap}.metric b{display:block;font-size:18px;color:var(--accent)}
main{height:calc(100vh - 122px);min-height:420px;position:relative}canvas{width:100%;height:100%;display:block}
.legend{position:absolute;top:12px;left:12px;padding:9px 11px;background:#081018dc;border:1px solid var(--line);border-radius:8px;color:var(--muted)}
.sw{display:inline-block;width:9px;height:9px;border-radius:50%;margin:0 4px 0 9px}.sw:first-child{margin-left:0}
footer{height:58px;display:grid;grid-template-columns:auto 1fr auto auto;gap:12px;align-items:center;padding:10px 18px;border-top:1px solid var(--line);background:var(--panel)}
button,select{background:#192a3b;color:var(--text);border:1px solid #36526b;border-radius:6px;padding:7px 10px}input[type=range]{width:100%}#clock{font-variant-numeric:tabular-nums;min-width:92px;text-align:right}
@media(max-width:760px){header{align-items:start;flex-direction:column}.metrics{gap:10px}main{height:calc(100vh - 190px)}footer{grid-template-columns:auto 1fr auto}#clock{display:none}}
</style>
</head>
<body>
<header><div><h1>{{.Title}}</h1><div class="meta">agent / job / assignment events — 汎用 rolling-window trajectory viewer</div></div>
<div class="metrics"><span class="metric"><b id="queue">0</b>waiting jobs</span><span class="metric"><b id="active">0</b>assigned</span><span class="metric"><b id="done">0</b>finished</span><span class="metric"><b id="dispatch">-</b>dispatch p95</span><span class="metric"><b id="reassign">0</b>reassignments</span></div></header>
<main><canvas id="map"></canvas><div class="legend"><span class="sw" style="background:var(--accent)"></span>agent <span class="sw" style="background:var(--job)"></span>pickup <span class="sw" style="background:var(--goal)"></span>destination<br>実線=現在の割当、薄線=直近の軌跡</div></main>
<footer><button id="play">▶</button><input id="time" type="range" min="0" max="1000" value="0"><span id="clock">00:00.0</span><select id="speed"><option value="0.5">0.5×</option><option value="1" selected>1×</option><option value="2">2×</option><option value="5">5×</option><option value="10">10×</option></select></footer>
<script>
"use strict";
const raw=Uint8Array.from(atob("{{.Payload}}"),c=>c.charCodeAt(0));
const data=JSON.parse(new TextDecoder().decode(raw));
const canvas=document.getElementById("map"),ctx=canvas.getContext("2d"),slider=document.getElementById("time");
const agents=new Map(data.agents.map(a=>{a.points=(a.points||[]).map(p=>({...p,t:Date.parse(p.at)}));return[a.id,a]}));
const jobs=data.jobs||[], assignments=data.assignments||[];
jobs.forEach(j=>{j.t=Date.parse(j.requested_at);j.end=j.finished_at?Date.parse(j.finished_at):Infinity});
assignments.forEach(a=>a.t=Date.parse(a.at));
let start=Infinity,end=-Infinity;const includeTime=t=>{start=Math.min(start,t);end=Math.max(end,t)};agents.forEach(a=>a.points.forEach(p=>includeTime(p.t)));jobs.forEach(j=>{includeTime(j.t);if(isFinite(j.end))includeTime(j.end)});assignments.forEach(a=>includeTime(a.t));
if(!isFinite(start)){start=0;end=1}const span=Math.max(1,end-start);
let now=start,playing=false,lastFrame=performance.now(),bounds;
function pointIndex(agent,t){const p=agent&&agent.points;if(!p||!p.length)return 0;let l=0,r=p.length;while(l<r){const m=(l+r)>>1;if(p[m].t<=t)l=m+1;else r=m}return l}
function position(agent,t){const i=pointIndex(agent,t);return i?agent.points[i-1]:null}
let assignmentCursor=0,assignmentTime=-Infinity,reassigns=0,currentAssignments=new Map;
function assignmentsAt(t){if(t<assignmentTime){assignmentCursor=0;reassigns=0;currentAssignments=new Map}while(assignmentCursor<assignments.length&&assignments[assignmentCursor].t<=t){const a=assignments[assignmentCursor++];if(currentAssignments.has(a.job_id))reassigns++;if(a.agent_id)currentAssignments.set(a.job_id,a.agent_id);else currentAssignments.delete(a.job_id)}assignmentTime=t;return[currentAssignments,reassigns]}
function extent(){let minX=Infinity,maxX=-Infinity,minY=Infinity,maxY=-Infinity;const add=p=>{minX=Math.min(minX,p.x);maxX=Math.max(maxX,p.x);minY=Math.min(minY,p.y);maxY=Math.max(maxY,p.y)};agents.forEach(a=>a.points.forEach(add));jobs.forEach(j=>{add(j.pickup);add(j.destination)});if(!isFinite(minX))return{minX:0,maxX:1,minY:0,maxY:1};const px=Math.max(1,(maxX-minX)*.04),py=Math.max(1,(maxY-minY)*.04);return{minX:minX-px,maxX:maxX+px,minY:minY-py,maxY:maxY+py}}
bounds=extent();
function project(p){const w=canvas.clientWidth,h=canvas.clientHeight,x=(p.x-bounds.minX)/(bounds.maxX-bounds.minX),y=(p.y-bounds.minY)/(bounds.maxY-bounds.minY);return[x*w,(1-y)*h]}
function resize(){const d=devicePixelRatio||1,w=canvas.clientWidth,h=canvas.clientHeight;if(canvas.width!==w*d||canvas.height!==h*d){canvas.width=w*d;canvas.height=h*d;ctx.setTransform(d,0,0,d,0,0)}}
function dot(p,color,r){const [x,y]=project(p);ctx.beginPath();ctx.fillStyle=color;ctx.arc(x,y,r,0,Math.PI*2);ctx.fill()}
function line(a,b,color,width=1){const p=project(a),q=project(b);ctx.beginPath();ctx.strokeStyle=color;ctx.lineWidth=width;ctx.moveTo(p[0],p[1]);ctx.lineTo(q[0],q[1]);ctx.stroke()}
function dispatchStats(){const values=[];for(const a of assignments){if(!a.agent_id)continue;const agent=agents.get(a.agent_id),job=jobsByID.get(a.job_id),p=position(agent,a.t);if(p&&job)values.push(Math.abs(p.x-job.pickup.x)+Math.abs(p.y-job.pickup.y))}values.sort((a,b)=>a-b);return values.length?values[Math.ceil(values.length*.95)-1].toFixed(0):"-"}
const jobsByID=new Map(jobs.map(j=>[j.id,j]));document.getElementById("dispatch").textContent=dispatchStats();
function draw(){resize();const w=canvas.clientWidth,h=canvas.clientHeight;ctx.clearRect(0,0,w,h);ctx.strokeStyle="#132638";ctx.lineWidth=1;for(let i=1;i<10;i++){ctx.beginPath();ctx.moveTo(w*i/10,0);ctx.lineTo(w*i/10,h);ctx.stroke();ctx.beginPath();ctx.moveTo(0,h*i/10);ctx.lineTo(w,h*i/10);ctx.stroke()}
 const [assigned,reassigns]=assignmentsAt(now);let waiting=0,active=0,done=0,shown=0;
 for(const j of jobs){if(j.t>now)continue;if(j.end<=now){done++;continue}const aid=assigned.get(j.id);if(aid)active++;else waiting++;if(shown++>500)continue;line(j.pickup,j.destination,"#fbbf2440");dot(j.destination,"#fbbf24",2);dot(j.pickup,aid?"#fb7185":"#94a3b8",3);if(aid){const p=position(agents.get(aid),now);if(p)line(p,j.pickup,"#fb7185b8",1.5)}}
 agents.forEach(a=>{const p=a.points;if(!p.length)return;const i=pointIndex(a,now),lo=Math.max(0,i-30);if(i-lo>1){ctx.beginPath();for(let k=lo;k<i;k++){const q=project(p[k]);if(k===lo)ctx.moveTo(q[0],q[1]);else ctx.lineTo(q[0],q[1])}ctx.strokeStyle="#5eead440";ctx.lineWidth=1;ctx.stroke()}const current=i?p[i-1]:null;if(current)dot(current,"#5eead4",3.5)});
 document.getElementById("queue").textContent=waiting;document.getElementById("active").textContent=active;document.getElementById("done").textContent=done;document.getElementById("reassign").textContent=reassigns;document.getElementById("clock").textContent=((now-start)/1000).toFixed(1)+"s";slider.value=Math.round((now-start)/span*1000)}
function frame(ts){if(playing){now+=Math.min(100,ts-lastFrame)*Number(document.getElementById("speed").value);if(now>=end){now=end;playing=false;document.getElementById("play").textContent="▶"}}lastFrame=ts;draw();requestAnimationFrame(frame)}
document.getElementById("play").onclick=()=>{if(now>=end)now=start;playing=!playing;document.getElementById("play").textContent=playing?"❚❚":"▶"};
slider.oninput=()=>{now=start+span*Number(slider.value)/1000;playing=false;document.getElementById("play").textContent="▶"};
window.addEventListener("resize",draw);requestAnimationFrame(frame);
</script>
</body></html>`))

// RenderHTML writes one portable file with no network or runtime dependency.
func RenderHTML(w io.Writer, dataset Dataset) error {
	if err := dataset.Normalize(); err != nil {
		return err
	}
	payload, err := json.Marshal(dataset)
	if err != nil {
		return fmt.Errorf("trajectoryviz: encode dataset: %w", err)
	}
	title := dataset.Title
	if title == "" {
		title = "isutools trajectory viewer"
	}
	if err := pageTemplate.Execute(w, struct {
		Title   string
		Payload string
	}{Title: title, Payload: base64.StdEncoding.EncodeToString(payload)}); err != nil {
		return fmt.Errorf("trajectoryviz: render HTML: %w", err)
	}
	return nil
}
