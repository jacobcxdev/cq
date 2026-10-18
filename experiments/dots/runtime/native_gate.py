"""Require a live-verified native principal before substituting owner credentials."""
import hashlib,hmac,threading,time
import catalog,settings

class NativeGate:
    def __init__(self,system_loader=None,upstream=None,clock=time.monotonic):
        self.system_loader=system_loader or (lambda:catalog.credential(catalog.read_owned(catalog.SYSTEM)))
        self.upstream=upstream or catalog.get
        self.clock=clock;self.lock=threading.Lock();self.cached=None

    def require(self,headers):
        values={k.lower():v for k,v in headers.items()}
        token=values.get('authorization','');account=values.get('chatgpt-account-id','')
        if not isinstance(token,str) or not token.startswith('Bearer ') or not 8<len(token)<=16384 or not isinstance(account,str) or not 1<len(account)<=256:
            raise ValueError('Native authentication required')
        fingerprint=hashlib.sha256((account+'\0'+token).encode()).digest()
        with self.lock:
            now=self.clock()
            if self.cached is not None and now<self.cached[1] and hmac.compare_digest(fingerprint,self.cached[0]):return
            system=self.system_loader()
            if system['alias']!=settings.SYSTEM_ALIAS or account!=system['account_id']:
                raise ValueError('Native principal mismatch')
            if settings.INTEGRATION_MODE == 'native':
                # Ordinary primary traffic passes through unchanged. Owner substitution
                # needs the actual native credential, independent of Dot availability.
                if not hmac.compare_digest(token[7:].encode(), system['token'].encode()):
                    raise ValueError('Native bearer mismatch')
                self.cached=(fingerprint,self.clock()+60)
                return
            native={**system,'token':token[7:]}
            value=self.upstream(native,'/backend-api/tbo/primary',headers)
            profile=value.get('profile',{});selection=value.get('selection',{})
            if (profile.get('id')!=catalog.PROOF_PROFILE_IDS[system['alias']]
                    or selection.get('available') is not True
                    or not isinstance(profile.get('active_root_thread_id'),str)
                    or selection.get('thread_id')!=profile['active_root_thread_id']):
                raise ValueError('Native principal verification failed')
            self.cached=(fingerprint,self.clock()+60)
