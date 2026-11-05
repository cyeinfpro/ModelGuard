import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {execFileSync} from 'node:child_process';

test('VERSION is the single release version and all generated metadata is current',async()=>{
 const version=(await readFile('VERSION','utf8')).trim();
 const readme=await readFile('README.md','utf8');
 assert.equal(readme.match(/当前工作版本：\*\*([^*]+)\*\*/)?.[1],version);
 assert.equal(JSON.parse(await readFile('package.json','utf8')).version,version);
 assert.equal(JSON.parse(await readFile('package-lock.json','utf8')).packages[''].version,version);
 execFileSync('python3',['scripts/sync-metadata.py','--check'],{encoding:'utf8'});
});
test('default model references and aliases are part of the versioned library',async()=>{
 const defaults=JSON.parse(await readFile('internal/metadata/defaults.json','utf8'));
 const library=JSON.parse(await readFile('internal/fingerprint/data/model_metadata.json','utf8'));
 const refs=new Set(library.models.map(m=>m.id));
 assert(refs.has(defaults.oauth_expected));
 assert(refs.has(defaults.api_expected));
 for(const value of Object.values(defaults.model_aliases))assert(refs.has(value));
});
