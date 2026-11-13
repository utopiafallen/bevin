// config-forms.js — the shared config FORM components, reused by the setup
// wizard and the admin config pages (/admin/models, /admin/email). Both pages
// define, before any of this runs: a global state object S (models, modelDraft,
// editingIndex, editOriginal, formError, aux, auxError, email), a global
// api(method, path, body), a render() (re-renders the page's form area), a
// msg(text, cls) status-line setter, and the endpoint constants PROBE_URL
// (model Detect) + EMAIL_TEST_URL (draft test send).
function el(tag, attrs, ...kids){ const e=document.createElement(tag); for(const [k,v] of Object.entries(attrs||{})){ if(k==="class")e.className=v; else if(k==="text")e.textContent=v; else e.setAttribute(k,v);} for(const c of kids){ if(c==null) continue; e.append(c.nodeType?c:document.createTextNode(c)); } return e; }
function msg(t, cls){ const m=document.getElementById("msg"); m.textContent=t||""; m.className="msg"+(cls?" "+cls:""); }
function field(label, input){ return el("div",{},el("label",{},label),input); }
function inp(id, val, ph){ const i=el("input",{id, value:val||"", placeholder:ph||""}); i.addEventListener("input",()=>sync(id,i.value)); return i; }
function pwfield(label, id, val, ph){ const i=el("input",{id, type:"password", value:val||"", placeholder:ph||""}); i.addEventListener("input",()=>sync(id,i.value)); const b=el("button",{type:"button", class:"pw-toggle"}, "Show"); b.onclick=function(){ const show=i.type==="password"; i.type=show?"text":"password"; b.textContent=show?"Hide":"Show"; b.setAttribute("aria-pressed", show?"true":"false"); }; const wrap=el("div",{class:"pw-wrap"},i,b); return el("div",{},el("label",{},label),wrap); }


// ---- model form helpers ----
function freshModel(){ return {provider:"",base_url:"",api_key:"",model_id:"",display_name:"",max_context:"",context_auto:false,metadata:{reasoning:5,knowledge:5,speed:5},slots:1,enabled:true,auto_disabled:false,lockedTo:null}; }
// base-URL normalization for "is this the same provider?" comparisons — match the
// server probe, which treats scheme/host variants and a trailing /v1 as the same
// endpoint (it tries /models and /v1/models). Trim, drop trailing slashes, drop /v1.
function normBase(u){ return (u||"").trim().replace(/\/+$/,"").replace(/\/v1$/i,""); }
// is this model id already added for this base URL (i.e. already in the model list for this provider)?
function modelAlreadyAdded(id, base){ return S.models.some(x => x.model_id===id && normBase(x.base_url)===normBase(base)); }
// allKnownBases — every base URL configured anywhere (main models + aux), deduped.
// Feeds the Base URL combobox so an endpoint entered in one step can be
// re-selected in another.
function allKnownBases(){ const s=new Set(); const add=x=>{ const u=(x.base_url||"").trim(); if(u)s.add(u); }; S.models.forEach(add); S.aux.forEach(add); return [...s]; }
// is this base URL already a configured provider (main or aux)? drives the
// live re-probe when re-selecting an existing provider from the Base URL
// combobox (detection results are never cached — the probe is always fresh).
function isKnownBase(base){ return allKnownBases().some(u => normBase(u)===normBase(base)); }
function resetDraft(){ S.modelDraft=freshModel(); S.editingIndex=-1; S.editOriginal=null; S.formError=null; }
function deepCopy(x){ return JSON.parse(JSON.stringify(x)); }
// lock the R/K/S + enabled to a display name's pre-existing values (so every model
// sharing a display name stays consistent); unlock when the name is new (keeps the
// current R/K/S + enabled)
function syncDispLock(){
  const m=S.modelDraft;
  const name=(m.display_name||"").trim();
  const match=S.models.find(x=>(x.display_name||"").trim()===name);
  if(match){
    if(m.lockedTo!==name){ m.metadata=deepCopy(match.metadata); m.enabled=match.enabled; m.lockedTo=name; }
  } else if(m.lockedTo!==null && m.lockedTo!==undefined){
    m.lockedTo=null; // unlock — keep the current R/K/S + enabled
  }
}
function suggestedFix(err){
  const e=String(err).toLowerCase();
  if(e.includes("401")||e.includes("403")||e.includes("unauthorized")||e.includes("forbidden")) return "check the API key.";
  if(e.includes("404")) return "check the base URL (the /models path was not found there).";
  if(e.includes("timeout")||e.includes("deadline")||e.includes("refused")||e.includes("connection")||e.includes("no such host")) return "check the base URL is reachable from this machine (correct host, port, and network).";
  if(e.includes("no models")) return "the endpoint is reachable but listed no models — check the base URL.";
  if(e.includes("could not parse")) return "the endpoint did not return a recognized /models list — check the base URL.";
  return "check the base URL (and API key if the provider needs one).";
}
function saveModel(){
  const m=S.modelDraft;
  if(!m.base_url){ S.formError="Base URL is required."; render(); return; }
  if(!m.model_id){ S.formError="Pick a model — press Detect to list the provider's models."; render(); return; }
  if(!m.display_name){ S.formError="Display name is required (what users will see)."; render(); return; }
  if(!m.max_context){ S.formError="Max context is required — the provider did not expose it, so enter it manually."; render(); return; }
  const wasEditing=S.editingIndex!==-1;
  const toSave=deepCopy(m); delete toSave._models; delete toSave._lastBase; delete toSave._detecting;
  // max_context is a string in the draft (from the input); the server's config
  // expects an int, so normalize it before the model enters S.models.
  const mc=parseInt(toSave.max_context,10);
  if(!Number.isFinite(mc) || mc<=0){ S.formError="Max context must be a positive integer (tokens)."; render(); return; }
  toSave.max_context=mc;
  // Execution slots: a whole number >= 1 (the server clamps to 1 anyway; the
  // form rejects invalid input so the error shows near the field).
  const sl=parseInt(toSave.slots,10);
  if(!Number.isFinite(sl) || sl<1){ S.formError="Execution slots must be a whole number >= 1."; render(); return; }
  toSave.slots=sl;
  if(!wasEditing){ S.models.push(toSave); } else { const idx=Math.min(S.editingIndex,S.models.length); S.models.splice(idx,0,toSave); }
  resetDraft(); render(); msg(wasEditing?"Model saved.":"Model added.","ok"); notifyConfigChanged("models");
}
function editModel(i){
  S.editOriginal=deepCopy(S.models[i]);
  S.modelDraft=deepCopy(S.models[i]);
  S.editingIndex=i; S.models.splice(i,1); S.formError=null;
  syncDispLock(); // lock the R/K/S + enabled if this display name is shared with another model
  render();
  const b=document.getElementById("mf_base"); if(b) b.scrollIntoView({behavior:"smooth",block:"center"});
}
function cancelEdit(){
  if(S.editingIndex!==-1 && S.editOriginal){
    const idx=Math.min(S.editingIndex,S.models.length);
    S.models.splice(idx,0,S.editOriginal);
  }
  resetDraft(); render();
}
function reorderModel(i,dir){
  const j=i+dir;
  if(j<0||j>=S.models.length) return;
  [S.models[i],S.models[j]]=[S.models[j],S.models[i]];
  render(); notifyConfigChanged("models");
}
// score slider — a progress bar whose fill goes red (1) → yellow (5) → green (10)
function scoreColor(v){ const hue = v<=5 ? (v-1)/4*60 : 60+(v-5)/5*60; return "hsl("+hue+",70%,45%)"; }
function scoreFill(input){ const v=parseInt(input.value,10)||1; input.style.setProperty("--score-color",scoreColor(v)); const f=(v-1)/9; input.style.setProperty("--val",f); const W=input.clientWidth; if(W>0){ input.style.setProperty("--pos",(f*(W-18)+9)+"px"); } }
function refreshScoreFills(){ requestAnimationFrame(function(){ document.querySelectorAll("input.score").forEach(scoreFill); }); }
let _scoreResize=null; window.addEventListener("resize",function(){ if(_scoreResize)clearTimeout(_scoreResize); _scoreResize=setTimeout(refreshScoreFills,120); });
function scoreSlider(label, id, val, onChange, disabled){ const v=val||5; const s=el("input",{type:"range", id, min:"1", max:"10", value:String(v), class:"score"}); if(disabled) s.setAttribute("disabled",""); scoreFill(s); const valEl=el("span",{class:"score-val"}, String(v)); s.addEventListener("input",()=>{ scoreFill(s); valEl.textContent=s.value; if(onChange)onChange(parseInt(s.value,10)); }); return el("div",{class:"score-field"}, el("label",{},label), el("div",{class:"score-row"}, s, valEl)); }
// probeDraft — call the probe endpoint for a draft (main or aux) and learn the
// provider + list the models (with modalities where the provider exposes
// them). The result is never cached: every detect is a live probe, so a
// provider's model list changes are picked up immediately. Shared by the
// main-model and aux steps.
async function probeDraft(draft){ const r=await api("POST", PROBE_URL, {base_url:draft.base_url, api_key:draft.api_key}); draft.provider=r.provider; draft._models=r.models; return r; }
// notifyConfigChanged — a page-level persistence hook (called by the shared
// form actions after they commit a change to S). The setup wizard leaves
// onConfigChanged undefined (it persists at step advance); the admin config
// pages set it to persist the change immediately (per action — no draft
// state, nothing to lose).
function notifyConfigChanged(area){ if (typeof onConfigChanged === "function") onConfigChanged(area); }
// applyModelFields — apply the detected models to a MAIN model draft: pick the
// first model not yet added for this provider + auto-fill its context (the
// original detectModel behavior).
function applyModelFields(m, models){ const avail=models.filter(mm=>!modelAlreadyAdded(mm.ID, m.base_url)); if(avail.length && !avail.some(x=>x.ID===m.model_id)){ m.model_id=avail[0].ID; if(avail[0].MaxContext){ m.max_context=String(avail[0].MaxContext); m.context_auto=true; } else { m.max_context=""; m.context_auto=false; } } }
// detect — call the probe endpoint, learn the provider, list the models + context lengths
async function detectModel(){ const m=S.modelDraft; if(m._detecting) return; if(!m.base_url){ S.formError="Enter the API base URL first."; render(); return; }
  // A new detect clears any prior detect error immediately (re-render now, not
  // after the probe resolves), and disables the Detect button for the duration.
  m._detecting=true; S.formError=null; msg("Detecting "+m.base_url+"…",""); render();
  try{ const r=await probeDraft(m); applyModelFields(m, r.models); const avail=r.models.filter(mm=>!modelAlreadyAdded(mm.ID, m.base_url)); msg("Detected "+r.provider+" — "+r.models.length+" model(s) found ("+avail.length+" not yet added).","ok"); }
  catch(e){ S.formError="Detect failed: "+e.message+" — "+suggestedFix(e.message); }
  finally{ m._detecting=false; render(); }
}
function freshAux(){ return {kinds:[], provider:"", base_url:"", api_key:"", model_id:""}; }
function addAux(){ S.aux.push(freshAux()); render(); notifyConfigChanged("aux"); }
function removeAux(i){ S.aux.splice(i,1); if(S.auxError) delete S.auxError[i]; render(); notifyConfigChanged("aux"); }
// modalitiesFromModel — the input modalities a detected model exposes (one or
// more of vision|audio|video), mapped from the probe's InputModalities. Returns
// [] when the provider doesn't expose them (e.g. OpenAI) → the user sets them
// manually. A single model may perceive several modalities (e.g. a VLM).
function modalitiesFromModel(mm){ const inM=(mm.InputModalities||[]).map(s=>String(s).toLowerCase()); const out=[]; if(inM.includes("image")||inM.includes("vision")) out.push("vision"); if(inM.includes("audio")) out.push("audio"); if(inM.includes("video")) out.push("video"); return out; }
// applyAuxFields — apply the detected models to an AUX draft: pick the first
// model that exposes a perception modality (else the first model) + set its
// kinds (one or more) where the provider exposes them. No dedupe — the same
// model may be used for several modalities.
function applyAuxFields(m, models, i){ const pick=(models.find(mm=>modalitiesFromModel(mm).length) || models[0]); if(pick){ m.model_id=pick.ID; const mods=modalitiesFromModel(pick); if(mods.length) m.kinds=mods; } }
// detectAux — probe the provider for aux model i, list its models, auto-pick a
// perception model + its modality. Mirrors detectModel.
async function detectAux(i){ const m=S.aux[i]; if(m._detecting) return; if(!m.base_url){ S.auxError=S.auxError||{}; S.auxError[i]="Enter the API base URL first."; render(); return; }
  // A new detect clears this card's prior detect error immediately (re-render
  // now) and disables its Detect button for the duration.
  m._detecting=true; if(S.auxError) S.auxError[i]=null; msg("Detecting "+m.base_url+"…",""); render();
  try{ const r=await probeDraft(m); applyAuxFields(m, r.models, i); msg("Detected "+r.provider+" — "+r.models.length+" model(s) found.","ok"); }
  catch(e){ S.auxError=S.auxError||{}; S.auxError[i]="Detect failed: "+e.message+" — "+suggestedFix(e.message); }
  finally{ m._detecting=false; render(); }
}

function renderModelForm(){
  const m=S.modelDraft, editing=S.editingIndex!==-1;
  const f=el("div",{});
  // half A: provider configuration (shared with the aux step)
  f.append(renderProviderHalf("mf", m, {
    knownBases: allKnownBases(),
    detect: () => detectModel(),
    clearBtn: { label: editing ? "Cancel" : "Clear", fn: cancelEdit },
    getError: () => S.formError,
    clearError: () => { S.formError = null; }
  }));
  // half B: in-agent UI configuration (what users see)
  const halfB=el("div",{class:"config-half"}, el("h4",{}, "In-agent UI (what users see)"));
  // Model (the provider's model id)
  const sel=el("select",{id:"mf_sel"});
  const allModels=m._models||[];
  const available=allModels.filter(mm=>!modelAlreadyAdded(mm.ID, m.base_url)); // hide models already added for this provider
  if(!allModels.length){ sel.append(el("option",{value:""},"— press Detect —")); }
  else if(!available.length){ sel.append(el("option",{value:"", disabled:true, selected:true}, "all "+allModels.length+" model(s) from this provider are already added")); }
  else { available.forEach(mm=>{ const o=el("option",{value:mm.ID}, mm.ID+(mm.MaxContext?"  ("+mm.MaxContext+" tokens)":"")); if(m.model_id===mm.ID) o.selected=true; sel.append(o); }); }
  sel.addEventListener("change",()=>{ m.model_id=sel.value; const mm=allModels.find(x=>x.ID===sel.value); if(mm&&mm.MaxContext){ m.max_context=String(mm.MaxContext); m.context_auto=true; } else { m.max_context=""; m.context_auto=false; } S.formError=null; render(); });
  // Max context
  const ctx=el("input",{id:"mf_ctx", value:m.max_context||"", class:m.context_auto?"ctx-disabled":null});
  if(m.context_auto) ctx.setAttribute("disabled","");
  ctx.addEventListener("input",()=>{ m.max_context=ctx.value; m.context_auto=false; S.formError=null; });
  // Display name — a combobox (text input + a datalist of the existing display names).
  // Picking an existing name inherits its R/K/S + enabled state and locks them, so all
  // models sharing a display name stay consistent.
  const disp=el("input",{id:"mf_disp", value:m.display_name||"", list:"disp_names", autocomplete:"off"});
  const dl=el("datalist",{id:"disp_names"});
  [...new Set(S.models.map(x=>(x.display_name||"").trim()).filter(Boolean))].forEach(n=>{ dl.append(el("option",{value:n})); });
  // Enable for users — a Yes/No segmented button
  const locked=(m.lockedTo!==null && m.lockedTo!==undefined);
  const en=el("div",{class:"yesno"});
  const yesB=el("button",{type:"button", class:m.enabled?"sel":""}, "Yes");
  const noB=el("button",{type:"button", class:!m.enabled?"sel":""}, "No");
  if(locked){ yesB.setAttribute("disabled",""); noB.setAttribute("disabled",""); }
  yesB.addEventListener("click",()=>{ if(locked)return; m.enabled=true; S.formError=null; render(); });
  noB.addEventListener("click",()=>{ if(locked)return; m.enabled=false; S.formError=null; render(); });
  en.append(yesB,noB);
  // Execution slots — per-model concurrency (scheduling, not a user-visible
  // knob; the admin dashboard visualizes each model's pool). The min-1 clamp
  // also lives in the server's config.Validate.
  const slotsInp = el("input", {id:"mf_slots", type:"number", min:"1", step:"1", value:String(m.slots||1), style:"max-width:6rem"});
  slotsInp.addEventListener("input", ()=>{ m.slots = slotsInp.value; S.formError=null; });
  halfB.append(
    field("Model", sel),
    // Max context — the help text sits under the label (above the input)
    el("div",{},
      el("label",{},"Max context"),
      el("p",{class:"field-help"},"The model's maximum context window in tokens — required so the agent knows how much history and prompt it can use before truncating or compacting. Auto-filled from the provider when it exposes it; enter it manually otherwise."),
      ctx),
    el("div",{class:"row"},
      field("Display name", el("div",{}, disp, dl)),
      field("Enable for users", en)),
    el("p",{class:"field-help"},"Shown to users to help them pick a model — informational only. Pick an existing name to reuse it: its scores + enabled state are inherited and locked so every model sharing a name stays consistent."),
    el("div",{},
      el("label",{},"Execution slots"),
      el("p",{class:"field-help"},"How many turns of this model may run at the same time (1 = strictly one at a time). Extra concurrent turns wait their turn in a queue."),
      slotsInp)
  );
  if(m.auto_disabled){ halfB.append(el("p",{class:"form-notice"},"This model is currently <strong>unavailable</strong> — the provider reported it missing, so it was auto-disabled and hidden from users. It re-enables automatically once a later attempt succeeds; you do not need to change anything.")); }
  halfB.append(
    el("h5",{class:"scores-header"},"Scores (1-10)"),
    el("p",{class:"field-help"},"Help the user gauge a model's strengths when picking one for a conversation — informational only; it does not change how the agent runs."),
    scoreSlider("Reasoning (1-10)","mf_r", m.metadata.reasoning||5, (v)=>{ m.metadata.reasoning=v; }, locked),
    scoreSlider("Knowledge (1-10)","mf_k", m.metadata.knowledge||5, (v)=>{ m.metadata.knowledge=v; }, locked),
    scoreSlider("Speed (1-10)","mf_s", m.metadata.speed||5, (v)=>{ m.metadata.speed=v; }, locked)
  );
  f.append(halfB);
  // the save button
  f.append(el("div",{class:"btns"}, el("button",{class:"primary", onclick:"saveModel()"}, editing?"Save changes":"+ Add model")));
  // wire the display-name input (base + key are wired inside renderProviderHalf)
  disp.addEventListener("input",()=>{ m.display_name=disp.value; });
  disp.addEventListener("change",()=>{ m.display_name=disp.value; syncDispLock(); S.formError=null; render(); });
  return f;
}
function renderModelList(){
  const w=el("div",{});
  if(!S.models.length){ w.append(el("p",{class:"empty-list"},"No models added yet — fill the form above and press \u201cAdd model\u201d.")); return w; }
  const t=el("table",{class:"model-table"});
  t.append(el("thead",{}, el("tr",{},
    el("th",{}, ""), el("th",{}, "Display name"), el("th",{}, "Provider"), el("th",{}, "Base URL"), el("th",{}, "Model ID"), el("th",{}, "Context"), el("th",{}, "Slots"), el("th",{}, "R/K/S"), el("th",{}, "On"), el("th",{}, ""))));
  const tb=el("tbody",{});
  S.models.forEach((m,i)=>{
    const up=el("button",{title:"Move up", onclick:`reorderModel(${i},-1)`}, "\u25b2"); if(i===0) up.setAttribute("disabled","");
    const dn=el("button",{title:"Move down", onclick:`reorderModel(${i},1)`}, "\u25bc"); if(i===S.models.length-1) dn.setAttribute("disabled","");
    tb.append(el("tr",{},
      el("td",{}, el("div",{class:"reorder"}, up, dn)),
      el("td",{}, el("span",{class:"kv", title:m.display_name||""}, m.display_name||"(unnamed)")),
      el("td",{}, m.provider||"—"),
      el("td",{}, el("span",{class:"kv baseurl", title:m.base_url||""}, m.base_url||"—")),
      el("td",{}, el("span",{class:"kv"}, m.model_id||"—")),
      el("td",{}, m.max_context?m.max_context:"auto"),
      el("td",{}, String(m.slots||1)),
      el("td",{}, (m.metadata.reasoning||0)+"/"+(m.metadata.knowledge||0)+"/"+(m.metadata.speed||0)),
      el("td",{}, m.auto_disabled ? el("span",{class:"badge-unavail", title:"The provider reported this model unavailable, so it was auto-disabled. It re-enables automatically when a later attempt succeeds."},"unavailable") : (m.enabled?"yes":"no")),
      el("td",{}, el("button",{class:"row-btn", onclick:`editModel(${i})`},"Edit"))));
  });
  t.append(tb);
  w.append(t);
  return w;
}

// renderProviderHalf — the shared "Provider configuration" block used by BOTH
// the main-model form and each aux model: a base URL (a combobox over the
// already-configured providers) + an API key + a Detect button + the detected
// provider label. `draft` is the object being edited (S.modelDraft or
// S.aux[i]); opts carries the form-specific callbacks:
//   knownBases  — base URLs for the datalist (main + aux, deduped)
//   detect      — the full probe (closure); called by the Detect button, on
//                 Enter in the Base URL field, and on a committed base-URL
//                 change for a known provider (always a live probe — detection
//                 results are never cached, so provider model changes are
//                 picked up immediately)
//   clearBtn    — { label, fn } or null (the ghost button beside Detect)
//   getError    — () => string|null (this half's inline error)
//   clearError  — () => void
function renderProviderHalf(prefix, draft, opts){
  const knownBases = opts.knownBases || [];
  const detect = opts.detect;
  const clearBtn = opts.clearBtn || null;
  const getError = opts.getError || null;
  const clearError = opts.clearError || null;
  const half = el("div", {class:"config-half"}, el("h4",{}, "Provider configuration"));
  const base = inp(prefix+"_base", draft.base_url, "https://api.openai.com");
  base.setAttribute("list", prefix+"_base_urls");
  const baseDl = el("datalist", {id:prefix+"_base_urls"});
  [...new Set(knownBases.filter(Boolean))].forEach(u => baseDl.append(el("option", {value:u})));
  const key = inp(prefix+"_key", draft.api_key, "blank for a local llama-server");
  const provLabel = draft.provider ? el("span", {class:"kv"}, draft.provider) : el("span", {class:"small"}, "— press Detect —");
  const btns = el("div", {class:"form-btns"});
  if(clearBtn){ const cb = el("button", {class:"ghost"}, clearBtn.label); cb.onclick = clearBtn.fn; btns.append(cb); }
  // The Detect button is disabled (with a "Detecting…" label) while this draft's
  // probe is in flight, so the user can't fire overlapping requests if the probe
  // takes a few seconds.
  const db = el("button", {class:"primary"}, draft._detecting ? "Detecting…" : "Detect");
  db.onclick = detect;
  if(draft._detecting){ db.setAttribute("disabled",""); db.style.opacity = ".6"; db.style.cursor = "default"; }
  btns.append(db);
  half.append(
    el("div", {class:"row"}, field("Base URL", el("div",{}, base, baseDl)), field("API key", key)),
    btns,
    field("Provider", provLabel)
  );
  if(getError && getError()){ half.append(el("p", {class:"form-err"}, getError())); }
  // Sync the base URL on every keystroke, but do NOT auto-detect — the probe
  // must not fire while the user is still typing.
  base.addEventListener("input", () => { draft.base_url = base.value; if(clearError) clearError(); });
  // Enter in the Base URL field triggers detection (both main + aux, for
  // consistency) — the quick way to probe a freshly typed endpoint.
  base.addEventListener("keydown", (ev) => { if(ev.key === "Enter"){ ev.preventDefault(); detect(); } });
  // Committing a value (blurring, or a combobox selection) for a KNOWN provider
  // re-probes it live — never from a cache, so a provider's model list changes
  // are always picked up. draft._lastBase (per form) avoids re-triggering on
  // the same value.
  base.addEventListener("change", () => {
    draft.base_url = base.value; if(clearError) clearError();
    const nb = (base.value||"").trim();
    const prev = draft._lastBase || "";
    draft._lastBase = base.value;
    if(nb && isKnownBase(nb) && normBase(nb) !== normBase(prev)) detect();
  });
  key.addEventListener("input", () => { draft.api_key = key.value; if(clearError) clearError(); });
  if(draft._lastBase === undefined) draft._lastBase = draft.base_url; // initial sync (no auto-detect of the current value)
  return half;
}

function renderAuxCard(i){
  const m = S.aux[i];
  const wrap = el("div", {class:"aux-block"});
  wrap.append(renderProviderHalf("af"+i, m, {
    knownBases: allKnownBases(),
    detect: () => detectAux(i),
    clearBtn: { label: "Clear", fn: () => { S.aux[i] = freshAux(); render(); } },
    getError: () => (S.auxError && S.auxError[i]) || null,
    clearError: () => { if(S.auxError) S.auxError[i] = null; }
  }));
  wrap.append(renderAuxPerception(i));
  wrap.append(el("div",{class:"btns"}, el("button",{class:"ghost", onclick:"removeAux("+i+")"},"Remove this aux model")));
  return wrap;
}
function renderAuxPerception(i){
  const m = S.aux[i];
  const ph = el("div", {class:"config-half"}, el("h4",{}, "Perception model"));
  const sel = el("select", {id:"a_sel_"+i});
  const allModels = m._models || [];
  if(!allModels.length){ sel.append(el("option", {value:""}, "— press Detect —")); }
  else { allModels.forEach(mm => { const mods = modalitiesFromModel(mm); const hint = mods.length ? "  ("+mods.join(", ")+")" : ""; const o = el("option", {value:mm.ID}, mm.ID + hint); if(m.model_id===mm.ID) o.selected=true; sel.append(o); }); }
  sel.addEventListener("change", () => { m.model_id = sel.value; const mm = allModels.find(x=>x.ID===sel.value); const mods = modalitiesFromModel(mm); if(mods.length) m.kinds = mods; render(); });
  const modWrap = el("div", {class:"modality-checks"});
  ["vision","audio","video"].forEach(v => {
    const cb = el("input", {type:"checkbox", id:"a_mod_"+i+"_"+v});
    if((m.kinds||[]).includes(v)) cb.checked = true;
    cb.addEventListener("change", () => { const s=new Set(m.kinds||[]); if(cb.checked) s.add(v); else s.delete(v); m.kinds=[...s]; });
    modWrap.append(el("label", {class:"modality-check"}, cb, v));
  });
  ph.append(
    field("Model", sel),
    field("Modalities (input)", modWrap),
    el("p",{class:"field-help"},"Which sense(s) this model perceives — pick one or more. Auto-filled from the model where the provider exposes them (llama-server does); set them manually otherwise. Its output is text."));
  return ph;
}

// renderAuxSection — the aux model cards + the Add button (no step heading:
// the page provides it). Shared by the setup wizard and the admin models page.
function renderAuxSection(){
  const d = el("div", {});
  if(!S.aux.length){ d.append(el("p", {class:"empty-list"},"No aux models yet — add one below.")); }
  S.aux.forEach((m,i) => d.append(renderAuxCard(i)));
  d.append(el("div",{class:"btns"},el("button",{onclick:"addAux()"},"+ Add aux model")));
  return d;
}

function renderEmailForm(){
  const e = S.email;
  const d = el("div",{});
  // Enable toggle (Yes/No).
  const en = el("div",{class:"yesno"});
  const yesB = el("button",{type:"button", class:e.enabled?"sel":""}, "Yes");
  const noB = el("button",{type:"button", class:!e.enabled?"sel":""}, "No");
  yesB.addEventListener("click",()=>{ e.enabled=true; render(); });
  noB.addEventListener("click",()=>{ e.enabled=false; render(); });
  en.append(yesB,noB);
  d.append(field("Enable email notifications", en));
  if(!e.enabled){
    d.append(el("p",{class:"small"},"Email is off — bevin will not send any email. The rest of this step is skipped."));
    return d;
  }
  // SMTP config (shown when enabled).
  d.append(el("h4",{style:"margin-top:1rem"},"SMTP server"));
  const host = inp("em_host", e.smtp_host, "smtp.example.com");
  host.addEventListener("input",()=>{ e.smtp_host=host.value; });
  const port = inp("em_port", e.smtp_port, "587");
  port.type = "number"; port.min = "1"; port.max = "65535";
  port.addEventListener("input",()=>{ e.smtp_port=port.value; });
  const user = inp("em_user", e.username, "you@example.com");
  user.addEventListener("input",()=>{ e.username=user.value; });
  const passInput = el("input",{id:"em_pw", type:"password", value:e.password||"", placeholder:"SMTP password / app password"});
  passInput.addEventListener("input",()=>{ e.password=passInput.value; });
  const passToggle = el("button",{type:"button", class:"pw-toggle"}, "Show");
  passToggle.onclick=function(){ const show=passInput.type==="password"; passInput.type=show?"text":"password"; passToggle.textContent=show?"Hide":"Show"; };
  const passWrap = el("div",{class:"pw-wrap"}, passInput, passToggle);
  const fromAddr = inp("em_from", e.from_address, "blank = the username");
  fromAddr.addEventListener("input",()=>{ e.from_address=fromAddr.value; });
  const fromName = inp("em_fromname", e.from_name, "bevin");
  fromName.addEventListener("input",()=>{ e.from_name=fromName.value; });
  // Encryption (STARTTLS / Off).
  const tls = el("div",{class:"yesno"});
  const tlsYes = el("button",{type:"button", class:e.use_starttls?"sel":""}, "STARTTLS");
  const tlsNo = el("button",{type:"button", class:!e.use_starttls?"sel":""}, "Off");
  tlsYes.addEventListener("click",()=>{ e.use_starttls=true; render(); });
  tlsNo.addEventListener("click",()=>{ e.use_starttls=false; render(); });
  tls.append(tlsYes,tlsNo);
  d.append(
    el("div",{class:"row"}, field("SMTP host", host), field("Port", port)),
    field("Username", user),
    el("div",{}, el("label",{}, "Password"), passWrap),
    el("div",{class:"row"}, field("From address", fromAddr), field("From name", fromName)),
    field("Encryption", tls),
    el("p",{class:"field-help"},"Port 587 + STARTTLS is the usual submission setup. A local relay may use 25 without STARTTLS — set Encryption to Off in that case (bevin still upgrades to TLS when the server offers it).")
  );
  // Test send — probe the SMTP settings (as typed, before saving) against a
  // real send. The draft SMTP config goes with the request (the server tests
  // it transiently; nothing is saved by this).
  d.append(el("h4",{style:"margin-top:1.2rem"},"Send a test email"));
  const testRow = el("div",{class:"form-btns"});
  const testTo = el("input", {id:"em_test_to", placeholder:"you@example.com"});
  const testBtn = el("button", {class:"primary"}, "Send test email");
  const testMsg = el("p", {class:"testmsg"});
  testBtn.onclick = async () => {
    const to = testTo.value.trim();
    if(!to){ testMsg.textContent = "Enter the address to send the test to."; testMsg.className = "testmsg err"; return; }
    testBtn.setAttribute("disabled",""); testBtn.style.opacity = ".6"; testMsg.textContent = "Sending…"; testMsg.className = "testmsg";
    const smtp = { enabled: e.enabled, smtp_host: e.smtp_host, smtp_port: parseInt(e.smtp_port, 10) || 0, username: e.username, password: e.password, from_address: e.from_address, from_name: e.from_name, use_starttls: e.use_starttls };
    try{
      await api("POST", EMAIL_TEST_URL, { to, smtp });
      testMsg.textContent = "Test email sent to " + to + " — check your inbox."; testMsg.className = "testmsg ok";
    }catch(err){
      testMsg.textContent = "Test send failed: " + err.message + " — check the SMTP settings."; testMsg.className = "testmsg err";
    }finally{
      testBtn.removeAttribute("disabled"); testBtn.style.opacity = "";
    }
  };
  testRow.append(testTo, testBtn);
  d.append(testRow, testMsg);
  return d;
}
