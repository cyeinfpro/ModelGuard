import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {existsSync} from 'node:fs';
import {execFileSync} from 'node:child_process';
import {createPublicKey,verify,createHash} from 'node:crypto';
import Ajv2020 from 'ajv/dist/2020.js';

test('release archive matches host schema, signature, and every file hash',{skip:!existsSync('dist/ModelGuard-0.2.1.s2plugin')},async()=>{
 const archive='dist/ModelGuard-0.2.1.s2plugin';
 const read=name=>execFileSync('unzip',['-p',archive,name],{maxBuffer:64*1024*1024});
 const raw=read('manifest.json');const manifest=JSON.parse(raw);
 const schema=JSON.parse(await readFile('internal/pluginv1/manifest.schema.json','utf8'));
 const ajv=new Ajv2020({strict:false});const validate=ajv.compile(schema);
 assert(validate(manifest),JSON.stringify(validate.errors));
 const signature=JSON.parse(read('signature.json'));const pub=Buffer.from((await readFile('dist/publisher-public-key.txt','utf8')).trim(),'base64');
 const key=createPublicKey({key:Buffer.concat([Buffer.from('302a300506032b6570032100','hex'),pub]),format:'der',type:'spki'});
 assert(verify(null,raw,key,Buffer.from(signature.signature,'base64')));
 for(const [name,hash] of Object.entries(manifest.files))assert.equal(createHash('sha256').update(read(name)).digest('hex'),hash,name);
 const tampered=Buffer.from(raw);tampered[10]^=1;
 assert.equal(verify(null,tampered,key,Buffer.from(signature.signature,'base64')),false);
});
