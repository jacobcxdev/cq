(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
const principal=await m.Ymr.safeGet('/tbo/primary');
if(principal.profile?.id!==PROFILE_PLACEHOLDER)throw Error('Native principal changed');
const state=await m.VAn().getState();
return {enabled:state.enabled,ready:state.ready,environment_id:state.environmentId};})()
