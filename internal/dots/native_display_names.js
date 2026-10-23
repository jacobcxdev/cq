(async()=>{const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');
const value=await m.Ymr.safeGet('/tbo');
return (value.items??[]).map(p=>({id:p.id,name:p.display_name}));})()