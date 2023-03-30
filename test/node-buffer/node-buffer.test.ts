import { assertNotEquals } from "https://deno.land/std@0.178.0/testing/asserts.ts";
import __14$ from "http://localhost:8080/v113/node_buffer.js";

Deno.test("node-buffer-file", () => {
    const { File } = __14$;
    assertNotEquals(typeof File, "undefined", "Missing File class in node.js api buffer");
});

Deno.test("node-buffer-blob", () => {
    const { Blob } = __14$;
    assertNotEquals(typeof Blob, "undefined", "Missing Blob class in node.js api buffer.");
});