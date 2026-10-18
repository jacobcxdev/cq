(async()=>{
        const m = await import('app://-/assets/MODULE');
        const key = Symbol.for('cq.dot-read-ownership.v2');
        if (globalThis[key]) return {installed:true,ownedReads:globalThis[key].ownedReads};
        const root = document.getElementById('root');
        const property = root && Object.keys(root).find(k=>k.startsWith('__reactContainer$'));
        const container = property && root[property];
        const fibres = [container?.current ?? container];
        const seen = new Set();
        let scope;
        const inspect = (value, depth) => {
            if (scope || !value || typeof value !== 'object' || seen.has(value)) return;
            seen.add(value);
            try {
                if (typeof value.get === 'function' && typeof value.watch === 'function'
                        && typeof value.get(m.GMr) === 'string'
                        && typeof value.get(m.XMr) === 'string' && value.get(m.JMr)) {
                    scope = value;
                    return;
                }
                if (depth > 0) for (const field of Object.values(Object.getOwnPropertyDescriptors(value)))
                    if ('value' in field) inspect(field.value, depth - 1);
            } catch {}
        };
        for (let i=0; i<fibres.length && i<10000 && !scope; i++) {
            const fibre = fibres[i];
            if (!fibre) continue;
            inspect(fibre.memoizedProps, 3);
            let hook = fibre.memoizedState;
            for (let j=0; hook && j<100 && !scope; j++,hook=hook.next)
                inspect(hook.memoizedState, 3);
            if (fibre.child) fibres.push(fibre.child);
            if (fibre.sibling) fibres.push(fibre.sibling);
        }
        if (!scope) throw Error('Native messaging scope unavailable');
        const cache = m.Lpr(scope);
        if (!cache || typeof cache.read !== 'function' || cache.disposed)
            throw Error('Native messaging cache unavailable');
        const catalogue = await m.Ymr.safeGet('/tbo');
        const expected = new Set(PROFILES);
        const profiles = catalogue.items ?? [];
        if (profiles.length !== expected.size || profiles.some(p=>!expected.delete(p.id)
                || typeof p.messaging_room_id !== 'string' || !p.messaging_room_id) || expected.size)
            throw Error('Native catalogue identity changed');
        const state = (GUARD)(cache, profiles.map(p=>p.messaging_room_id), m.Ymr);
        globalThis[key] = state;
        return {installed:true,ownedReads:state.ownedReads};
    })()