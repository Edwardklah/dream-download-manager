// Images imported by the code become URLs (vite does this).
declare module "*.png" {
  const url: string;
  export default url;
}
