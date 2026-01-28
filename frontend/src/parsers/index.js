/**
 * @fileoverview File Parsers
 */

//Imports
import {FileFormats} from 'unified-3d-loader';
import gcodeParser from './gcode';
import unifiedParser from './unified';

//Export
export default async (file, extension, transfer, theme, progress) => {
  const ext = (extension || "").toLowerCase();

  // First: let unified-3d-loader handle 3D formats it knows
  for (const format of Object.values(FileFormats)) {
    if (format.extensions.includes(ext)) {
      const meshes = await unifiedParser(file, format, transfer, theme, progress);
      return meshes;
    }
  }

  // GCODE + CNC variants
  const gcodeExts = ["gcode", "nc", "tap", "cnc"];
  if (gcodeExts.includes(ext)) {
    const lines = await gcodeParser(file, transfer, theme, progress);
    return lines;
  }

  // If we get here, nothing matched — optionally log
  console.warn("[3D viewer] no parser for extension:", ext);
};