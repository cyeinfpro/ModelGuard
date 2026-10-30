import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {existsSync} from 'node:fs';
import {execFileSync} from 'node:child_process';
import {createPublicKey,verify,createHash} from 'node:crypto';
import Ajv2020 from 'ajv/dist/2020.js';

const version=(await readFile('VERSION','utf8')).trim();
const archive='dist/ModelGuard-'+version+'.s2plugin';

test('release archive matches host schema, declared integrity, and its signature state',{skip:!existsSync(archive)},async()=>{
 const read=name=>execFileSync('unzip',['-p',archive,name],{maxBuffer:64*1024*1024});
 const entries=execFileSync('unzip',['-Z1',archive],{encoding:'utf8'}).trim().split('\n');
 assert.equal(new Set(entries).size,entries.length,'archive paths must be unique');
 const raw=read('manifest.json');const manifest=JSON.parse(raw);
 assert.equal(manifest.version,version,'the package must match the current source version');
 const schema=JSON.parse(await readFile('internal/pluginv1/manifest.schema.json','utf8'));
 const validate=new Ajv2020({strict:false}).compile(schema);
 assert(validate(manifest),JSON.stringify(validate.errors));
 for(const [name,hash] of Object.entries(manifest.files))assert.equal(createHash('sha256').update(read(name)).digest('hex'),hash,name);
 const signed=entries.includes('signature.json');
 assert.deepEqual([...entries].sort(),['manifest.json',...(signed?['signature.json']:[]),...Object.keys(manifest.files)].sort(),'every payload file must be declared');
 if(signed){
  const signature=JSON.parse(read('signature.json'));
  assert.equal(signature.algorithm,'ed25519');assert(signature.key_id?.trim(),'signed packages need a publisher id');
  const pub=Buffer.from((await readFile('dist/publisher-public-key.txt','utf8')).trim(),'base64');
  assert.equal(pub.length,32);
  const key=createPublicKey({key:Buffer.concat([Buffer.from('302a300506032b6570032100','hex'),pub]),format:'der',type:'spki'});
  assert(verify(null,raw,key,Buffer.from(signature.signature,'base64')),'present signatures must verify');
  const tampered=Buffer.from(raw);tampered[10]^=1;
  assert.equal(verify(null,tampered,key,Buffer.from(signature.signature,'base64')),false);
 }
});
